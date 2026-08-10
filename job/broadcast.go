// Package job — сама регулярная задача: разослать вопрос всем пользователям.
package job

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Ivanvnew75/libs/common"
)

type User struct {
	ID         int64  `json:"id"`
	TelegramID *int64 `json:"telegram_id,omitempty"`
	Name       string `json:"name"`
}

type sendRequest struct {
	TelegramID int64  `json:"telegram_id"`
	Text       string `json:"text"`
}

type Broadcaster struct {
	usersURL    string
	telegramURL string
	client      *common.Client
	locker      *Locker
	log         *slog.Logger
	question    string

	// Бизнес-метрики (Фактор 13).
	//
	// Технические метрики (rps, задержки) отвечают на вопрос «работает ли
	// сервис». Бизнес-метрики отвечают на вопрос «делает ли он то, ради
	// чего существует». Здесь это принципиально: scheduler может быть
	// полностью здоров по всем HTTP-метрикам и при этом не разослать
	// ни одного вопроса — например, если блокировка зависла или список
	// пользователей пуст. Технические метрики этого НЕ покажут.
	sent    prometheus.Counter
	failed  prometheus.Counter
	skipped prometheus.Counter
	lastRun prometheus.Gauge
}

func NewBroadcaster(usersURL, telegramURL string, client *common.Client, locker *Locker, log *slog.Logger, question string) *Broadcaster {
	const ns = "moodbot"
	return &Broadcaster{
		usersURL: usersURL, telegramURL: telegramURL,
		client: client, locker: locker, log: log, question: question,

		sent: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "broadcast_messages_sent_total",
			Help: "Сколько вопросов успешно отправлено",
		}),
		failed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "broadcast_messages_failed_total",
			Help: "Сколько отправок не удалось",
		}),
		skipped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Name: "broadcast_skipped_total",
			Help: "Сколько рассылок пропущено из-за блокировки другой реплики",
		}),
		// Gauge с временем последнего запуска, а не Counter запусков.
		//
		// По нему пишется главный алерт этого сервиса:
		//   time() - moodbot_broadcast_last_run_timestamp_seconds > 13*3600
		// «рассылки не было дольше, чем должно» — то есть алерт
		// на ОТСУТСТВИЕ события. Счётчиком такое не выразить:
		// counter, который перестал расти, выглядит ровно как counter,
		// по которому просто нет трафика.
		lastRun: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Name: "broadcast_last_run_timestamp_seconds",
			Help: "Время последней завершённой рассылки, unix seconds",
		}),
	}
}

// RegisterMetrics добавляет бизнес-метрики в общий регистр сервиса.
func (b *Broadcaster) RegisterMetrics(reg *prometheus.Registry) {
	reg.MustRegister(b.sent, b.failed, b.skipped, b.lastRun)
}

// lastRunKey — где хранится время последней рассылки.
const lastRunKey = "moodbot:broadcast:last_run"

// RestoreLastRun поднимает время последней рассылки из Redis при старте.
//
// ЗАЧЕМ ЭТО НУЖНО — найдено проверкой, а не придумано.
//
// Gauge, зарегистрированный и не заполненный, экспортируется со значением 0.
// Алерт `time() - broadcast_last_run_timestamp_seconds > 13h` при нуле
// даёт «прошло 496 210 часов» и срабатывает СРАЗУ ПОСЛЕ КАЖДОЙ ВЫКАТКИ.
// Ложная тревога после каждого деплоя — быстрый способ приучить дежурного
// игнорировать алерты этого сервиса.
//
// Причина глубже, чем «забыли инициализировать»: состояние «когда была
// последняя рассылка» хранилось в памяти процесса и терялось при рестарте.
// Это ровно нарушение Фактора 6. Правильное место для него — backing
// service, тот же Redis, который уже используется для блокировки.
//
// Запасной вариант (Redis пуст, первый запуск в жизни) — время старта
// процесса: тогда алерт даст отсрочку в свои 13 часов, а не выстрелит.
func (b *Broadcaster) RestoreLastRun(ctx context.Context) {
	ts, err := b.locker.GetLastRun(ctx, lastRunKey)
	switch {
	case err != nil:
		b.log.Warn("не удалось прочитать время последней рассылки",
			slog.String("error", err.Error()))
		b.lastRun.SetToCurrentTime()
	case ts.IsZero():
		b.log.Info("время последней рассылки неизвестно, беру время старта")
		b.lastRun.SetToCurrentTime()
	default:
		b.lastRun.Set(float64(ts.Unix()))
		b.log.Info("восстановлено время последней рассылки", slog.Time("at", ts))
	}
}

type Result struct {
	Skipped bool `json:"skipped"`
	Total   int  `json:"total"`
	Sent    int  `json:"sent"`
	Failed  int  `json:"failed"`
}

// Run выполняет одну рассылку.
//
// slot — идентификатор «окна» рассылки, он же ключ блокировки.
// В нём есть дата и час: блокировка защищает конкретный запуск,
// а не «рассылку вообще». Иначе вечерняя рассылка не состоялась бы,
// если утренняя оставила ключ.
func (b *Broadcaster) Run(ctx context.Context, slot string) (Result, error) {
	lockKey := "moodbot:broadcast:" + slot

	release, ok, err := b.locker.Acquire(ctx, lockKey)
	if err != nil {
		return Result{}, fmt.Errorf("acquire lock: %w", err)
	}
	if !ok {
		// Не ошибка: другая реплика уже делает эту работу.
		b.skipped.Inc()
		b.log.Info("рассылка пропущена — блокировку держит другая реплика",
			slog.String("slot", slot))
		return Result{Skipped: true}, nil
	}
	defer release()

	users, err := b.fetchAllUsers(ctx)
	if err != nil {
		return Result{}, err
	}

	res := Result{Total: len(users)}
	for _, u := range users {
		if u.TelegramID == nil {
			// Пользователь без telegram_id (заведён через API напрямую) —
			// слать некуда, это не ошибка.
			continue
		}

		// Таймаут на КАЖДУЮ отправку отдельно. Общий таймаут на всю
		// рассылку означал бы, что при тысяче пользователей последние
		// не получат ничего.
		sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := b.client.DoJSON(sendCtx, http.MethodPost, b.telegramURL+"/send",
			sendRequest{TelegramID: *u.TelegramID, Text: b.question}, nil)
		cancel()

		if err != nil {
			// Одна неудачная отправка не должна ронять всю рассылку.
			b.failed.Inc()
			res.Failed++
			b.log.Error("отправка не удалась",
				slog.Int64("user_id", u.ID),
				slog.String("error", err.Error()))
			continue
		}
		b.sent.Inc()
		res.Sent++
	}

	now := time.Now()
	b.lastRun.Set(float64(now.Unix()))
	// Пишем в Redis, чтобы значение пережило рестарт пода.
	if err := b.locker.SetLastRun(ctx, lastRunKey, now); err != nil {
		b.log.Warn("не удалось сохранить время рассылки",
			slog.String("error", err.Error()))
	}

	b.log.Info("рассылка завершена",
		slog.String("slot", slot),
		slog.Int("total", res.Total),
		slog.Int("sent", res.Sent),
		slog.Int("failed", res.Failed))
	return res, nil
}

// fetchAllUsers обходит постраничный список.
//
// Постранично, а не «дай всех сразу»: сервис users намеренно ограничивает
// limit. Клиент, рассчитывающий получить всё одним запросом, сломается
// ровно тогда, когда пользователей станет много, — то есть в самый
// неудачный момент.
func (b *Broadcaster) fetchAllUsers(ctx context.Context) ([]User, error) {
	const pageSize = 100
	var all []User

	for offset := 0; ; offset += pageSize {
		var page []User
		url := fmt.Sprintf("%s/users?limit=%d&offset=%d", b.usersURL, pageSize, offset)
		if err := b.client.DoJSON(ctx, http.MethodGet, url, nil, &page); err != nil {
			return nil, fmt.Errorf("fetch users: %w", err)
		}
		all = append(all, page...)
		if len(page) < pageSize {
			return all, nil
		}
	}
}

// Slot возвращает идентификатор окна рассылки для момента t.
// Гранулярность — час: два запуска в один и тот же час считаются
// одним окном, что и защищает от дублей при перезапуске пода.
func Slot(t time.Time) string { return t.Format("2006-01-02T15") }
