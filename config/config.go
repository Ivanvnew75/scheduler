package config

import (
	"time"

	"github.com/Ivanvnew75/libs/common"
)

type Config struct {
	Port    string
	Service string
	Version string

	// RedisURL — backing service для распределённой блокировки (Фактор 4).
	RedisURL string

	UsersServiceURL    string
	TelegramServiceURL string

	// Schedule — cron-выражение. В конфигурации, а не в коде: поменять
	// время рассылки должно быть правкой ConfigMap и рестартом пода,
	// а не выкладкой новой версии.
	Schedule string
	// Timezone — обязательно явно.
	//
	// Контейнер по умолчанию живёт в UTC. «Рассылка в 9 утра» без указания
	// зоны означает 9 утра UTC, то есть полдень в Москве. Это классическая
	// ошибка, которую замечают только пользователи.
	Timezone string

	Question string

	// LockTTL — на сколько берётся блокировка в Redis.
	LockTTL time.Duration

	LogLevel        string
	LogFormat       string
	ShutdownTimeout time.Duration
	HTTPTimeout     time.Duration
	HTTPRetries     int
}

func Load(version string) (Config, error) {
	c := Config{
		Port:    common.Env("SERVER_PORT", "8080"),
		Service: "scheduler",
		Version: version,
		// Два раза в день: 9:00 и 21:00. Формат — стандартный cron
		// из пяти полей (минута час день месяц день_недели).
		Schedule:  common.Env("SCHEDULE", "0 9,21 * * *"),
		Timezone:  common.Env("TZ", "Europe/Moscow"),
		Question:  common.Env("MOOD_QUESTION", "Как вы себя чувствуете?"),
		LogLevel:  common.Env("LOG_LEVEL", "info"),
		LogFormat: common.Env("LOG_FORMAT", "json"),
	}

	var err error
	if c.RedisURL, err = common.MustEnv("REDIS_URL"); err != nil {
		return c, err
	}
	if c.UsersServiceURL, err = common.MustEnv("USERS_SERVICE_URL"); err != nil {
		return c, err
	}
	if c.TelegramServiceURL, err = common.MustEnv("TELEGRAM_SERVICE_URL"); err != nil {
		return c, err
	}
	// TTL блокировки должен быть заметно больше, чем длится рассылка,
	// иначе блокировка истечёт на середине и вторая реплика начнёт
	// слать вопросы повторно.
	if c.LockTTL, err = common.EnvDuration("LOCK_TTL", 5*time.Minute); err != nil {
		return c, err
	}
	if c.ShutdownTimeout, err = common.EnvDuration("SHUTDOWN_TIMEOUT", 15*time.Second); err != nil {
		return c, err
	}
	if c.HTTPTimeout, err = common.EnvDuration("HTTP_TIMEOUT", 10*time.Second); err != nil {
		return c, err
	}
	if c.HTTPRetries, err = common.EnvInt("HTTP_RETRIES", 2); err != nil {
		return c, err
	}
	return c, nil
}
