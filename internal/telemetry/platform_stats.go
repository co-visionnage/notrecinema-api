package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
)

// Querier -- то, что нужно сборщику от БД; *postgres.Pool удовлетворяет.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PlatformStats -- сводные числа по платформе: сколько в ней людей, семей,
// сериалов, активных сессий. Это то, что на дашборде отвечает на вопрос
// «растёт ли сервис и не залипло ли что-то»; личных данных здесь нет.
type PlatformStats struct {
	Users              int64
	UsersVerified      int64
	UsersWithTwoFactor int64
	Families           int64
	Series             int64
	ActiveSessions     int64
	PushSubscriptions  int64
	PendingInvitations int64
}

const platformStatsTTL = 30 * time.Second

// PlatformStatsCollector читает get_platform_stats_system() (миграция 0039)
// не чаще раза в 30 секунд: подсчёт строк не нужно повторять на каждый
// скрейп, а при нескольких репликах каждая отдаёт одно и то же значение.
type PlatformStatsCollector struct {
	db     Querier
	logger *slog.Logger

	mu        sync.Mutex
	stats     PlatformStats
	fetchedAt time.Time
	hasStats  bool

	users, families, series, sessions, pushSubs, invitations *prometheus.Desc
	scrapeErrors                                             prometheus.Counter
}

func NewPlatformStatsCollector(db Querier, logger *slog.Logger) *PlatformStatsCollector {
	return &PlatformStatsCollector{
		db:     db,
		logger: logger,
		users: prometheus.NewDesc("platform_users",
			"Пользователи платформы по состоянию (all/verified/two_factor).", []string{"state"}, nil),
		families:    prometheus.NewDesc("platform_families", "Количество семей.", nil, nil),
		series:      prometheus.NewDesc("platform_series", "Количество сериалов во всех семьях.", nil, nil),
		sessions:    prometheus.NewDesc("platform_active_sessions", "Действующие сессии входа.", nil, nil),
		pushSubs:    prometheus.NewDesc("platform_push_subscriptions", "Подписки на web push.", nil, nil),
		invitations: prometheus.NewDesc("platform_pending_invitations", "Неотвеченные приглашения в семьи.", nil, nil),
		scrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "platform_stats_errors_total",
			Help: "Сколько раз не удалось прочитать сводные числа платформы из БД.",
		}),
	}
}

func (c *PlatformStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.users
	ch <- c.families
	ch <- c.series
	ch <- c.sessions
	ch <- c.pushSubs
	ch <- c.invitations
	c.scrapeErrors.Describe(ch)
}

func (c *PlatformStatsCollector) refresh() {
	if c.hasStats && time.Since(c.fetchedAt) < platformStatsTTL {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var s PlatformStats
	err := c.db.QueryRow(ctx, `
		SELECT users, users_verified, users_with_two_factor, families, series,
		       active_sessions, push_subscriptions, pending_invitations
		FROM public.get_platform_stats_system()
	`).Scan(&s.Users, &s.UsersVerified, &s.UsersWithTwoFactor, &s.Families, &s.Series,
		&s.ActiveSessions, &s.PushSubscriptions, &s.PendingInvitations)
	if err != nil {
		c.scrapeErrors.Inc()
		if c.logger != nil {
			c.logger.Warn("telemetry: не удалось прочитать сводные числа платформы", "error", err)
		}
		return
	}

	c.stats, c.fetchedAt, c.hasStats = s, time.Now(), true
}

func (c *PlatformStatsCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	c.refresh()
	s, ok := c.stats, c.hasStats
	c.mu.Unlock()

	c.scrapeErrors.Collect(ch)
	if !ok {
		return
	}

	ch <- prometheus.MustNewConstMetric(c.users, prometheus.GaugeValue, float64(s.Users), "all")
	ch <- prometheus.MustNewConstMetric(c.users, prometheus.GaugeValue, float64(s.UsersVerified), "verified")
	ch <- prometheus.MustNewConstMetric(c.users, prometheus.GaugeValue, float64(s.UsersWithTwoFactor), "two_factor")
	ch <- prometheus.MustNewConstMetric(c.families, prometheus.GaugeValue, float64(s.Families))
	ch <- prometheus.MustNewConstMetric(c.series, prometheus.GaugeValue, float64(s.Series))
	ch <- prometheus.MustNewConstMetric(c.sessions, prometheus.GaugeValue, float64(s.ActiveSessions))
	ch <- prometheus.MustNewConstMetric(c.pushSubs, prometheus.GaugeValue, float64(s.PushSubscriptions))
	ch <- prometheus.MustNewConstMetric(c.invitations, prometheus.GaugeValue, float64(s.PendingInvitations))
}
