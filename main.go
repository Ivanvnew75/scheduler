package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"

	"github.com/Ivanvnew75/libs/common"

	"github.com/Ivanvnew75/scheduler/api"
	"github.com/Ivanvnew75/scheduler/config"
	"github.com/Ivanvnew75/scheduler/job"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	cfg, err := config.Load(version)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	logger := common.NewLogger(cfg.Service, cfg.Version, cfg.LogFormat, cfg.LogLevel)

	// Часовой пояс загружаем ЯВНО и падаем, если его нет.
	//
	// В минимальных образах (scratch, distroless без tzdata) базы зон нет,
	// и time.LoadLocation молча вернёт ошибку. Тогда расписание уехало бы
	// в UTC — рассылка в неправильное время, и никакого сообщения об этом.
	// Поэтому в Dockerfile стоит apk add tzdata, а здесь — fail fast.
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		logger.Error("не удалось загрузить часовой пояс — проверьте, что в образе есть tzdata",
			slog.String("tz", cfg.Timezone), slog.String("error", err.Error()))
		log.Fatal("timezone error")
	}

	logger.Info("starting",
		slog.String("commit", commit),
		slog.String("schedule", cfg.Schedule),
		slog.String("timezone", cfg.Timezone))

	// Redis как backing service: адрес приходит строкой из конфига.
	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Fatalf("REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()

	locker := job.NewLocker(rdb, cfg.LockTTL)
	client := common.NewClient(cfg.HTTPTimeout, cfg.HTTPRetries)
	broadcaster := job.NewBroadcaster(
		cfg.UsersServiceURL, cfg.TelegramServiceURL,
		client, locker, logger, cfg.Question,
	)

	ctx, stop := common.SignalContext()
	defer stop()

	// cron с явной зоной, а не в локальном времени процесса.
	c := cron.New(cron.WithLocation(loc), cron.WithLogger(cron.DiscardLogger))
	if _, err := c.AddFunc(cfg.Schedule, func() {
		// Контекст запуска не наследует ctx напрямую: если задача
		// стартовала, её лучше довести до конца, а не рвать на середине
		// при получении SIGTERM. Ограничение — собственный таймаут.
		runCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		slot := job.Slot(time.Now().In(loc))
		if _, err := broadcaster.Run(runCtx, slot); err != nil {
			logger.Error("рассылка не удалась",
				slog.String("slot", slot), slog.String("error", err.Error()))
		}
	}); err != nil {
		log.Fatalf("некорректное расписание %q: %v", cfg.Schedule, err)
	}
	c.Start()

	// Показываем время следующего запуска в логе при старте.
	// Мелочь, которая экономит много времени: сразу видно, что
	// расписание разобрано именно так, как задумано.
	if entries := c.Entries(); len(entries) > 0 {
		logger.Info("следующая рассылка",
			slog.Time("at", entries[0].Next),
			slog.String("timezone", cfg.Timezone))
	}

	srv := api.New(broadcaster, locker, logger, loc).Echo()
	go func() {
		addr := ":" + cfg.Port
		logger.Info("http server listening", slog.String("addr", addr))
		if err := srv.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", slog.String("error", err.Error()))
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")

	// c.Stop() возвращает контекст, который закроется, когда доработают
	// уже запущенные задачи. Без ожидания рассылка оборвалась бы
	// на середине — часть пользователей получила бы вопрос, часть нет.
	cronCtx := c.Stop()

	shutdownCtx, cancel := common.ShutdownContext(cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
	}

	select {
	case <-cronCtx.Done():
		logger.Info("активные задачи завершены")
	case <-shutdownCtx.Done():
		logger.Warn("задачи не успели завершиться за отведённое время")
	}
	logger.Info("stopped")
}
