// Package api — служебный HTTP-интерфейс scheduler'а.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/Ivanvnew75/libs/common"

	"github.com/Ivanvnew75/scheduler/job"
)

type Server struct {
	b      *job.Broadcaster
	locker *job.Locker
	log    *slog.Logger
	tz     *time.Location
}

func New(b *job.Broadcaster, locker *job.Locker, log *slog.Logger, tz *time.Location) *Server {
	return &Server{b: b, locker: locker, log: log, tz: tz}
}

func (s *Server) Echo() *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(common.RequestID())
	e.Use(common.PropagateRequestID())
	e.Use(common.RequestLogger(s.log))
	e.Use(middleware.Recover())

	e.GET("/health", s.health)
	e.GET("/ready", s.ready)
	e.POST("/trigger", s.trigger)
	return e
}

func (s *Server) health(c echo.Context) error {
	return c.JSON(http.StatusOK, echo.Map{"status": "ok"})
}

// ready проверяет Redis: без него нельзя взять блокировку,
// а значит нельзя безопасно выполнить рассылку.
func (s *Server) ready(c echo.Context) error {
	if err := s.locker.Ping(c.Request().Context()); err != nil {
		return c.JSON(http.StatusServiceUnavailable, echo.Map{
			"status": "unavailable", "reason": "redis unreachable",
		})
	}
	return c.JSON(http.StatusOK, echo.Map{"status": "ready"})
}

// trigger — ручной запуск рассылки.
//
// Зачем: ждать 9 утра, чтобы проверить, что рассылка работает, —
// плохая идея. Это административная операция (мостик к Фактору 12),
// и она вынесена в ручку, а не в отдельный бинарник, чтобы выполняться
// в том же процессе с тем же кодом и той же конфигурацией.
//
// Ручка НЕ выставлена наружу: Service — ClusterIP, доступ через
// kubectl port-forward или изнутри кластера. Для настоящего прода
// сюда нужна аутентификация — это осознанное упрощение учебного стенда.
func (s *Server) trigger(c echo.Context) error {
	// Отдельный слот с суффиксом manual: ручной запуск не должен
	// «съесть» блокировку регулярного и отменить плановую рассылку.
	slot := job.Slot(time.Now().In(s.tz)) + "-manual-" + time.Now().Format("050405")

	res, err := s.b.Run(c.Request().Context(), slot)
	if err != nil {
		s.log.Error("ручная рассылка не удалась", slog.String("error", err.Error()))
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, res)
}
