// Package job — сама регулярная задача: разослать вопрос всем пользователям.
package job

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

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
}

func NewBroadcaster(usersURL, telegramURL string, client *common.Client, locker *Locker, log *slog.Logger, question string) *Broadcaster {
	return &Broadcaster{
		usersURL: usersURL, telegramURL: telegramURL,
		client: client, locker: locker, log: log, question: question,
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
			res.Failed++
			b.log.Error("отправка не удалась",
				slog.Int64("user_id", u.ID),
				slog.String("error", err.Error()))
			continue
		}
		res.Sent++
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
