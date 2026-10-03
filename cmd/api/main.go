// Команда api запускает HTTP API notrecinema: первый вынесенный из
// Next.js-монолита слой бэкенд-логики. Работает с той же базой Postgres и
// той же cookie сессии, поэтому может разворачиваться рядом с монолитом и
// постепенно забирать себе роуты.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"notrecinema/api/internal/activitylog"
	"notrecinema/api/internal/auth"
	"notrecinema/api/internal/calendar"
	"notrecinema/api/internal/config"
	"notrecinema/api/internal/digest"
	"notrecinema/api/internal/eventbus"
	"notrecinema/api/internal/events"
	"notrecinema/api/internal/export"
	"notrecinema/api/internal/families"
	"notrecinema/api/internal/history"
	"notrecinema/api/internal/httpserver"
	"notrecinema/api/internal/imports"
	"notrecinema/api/internal/logging"
	"notrecinema/api/internal/movieprovider"
	"notrecinema/api/internal/movies"
	"notrecinema/api/internal/notifications"
	"notrecinema/api/internal/nudges"
	"notrecinema/api/internal/oauth"
	"notrecinema/api/internal/outbox"
	"notrecinema/api/internal/polls"
	"notrecinema/api/internal/postgres"
	"notrecinema/api/internal/seasons"
	"notrecinema/api/internal/series"
	"notrecinema/api/internal/stats"
	"notrecinema/api/internal/telemetry"
	"notrecinema/api/internal/upload"
	"notrecinema/api/internal/users"
)

func main() {
	logger := logging.New("notrecinema-api")

	if err := run(logger); err != nil {
		logger.Error("fatal startup error", "error", err)
		os.Exit(1)
	}
}

// version подставляется при сборке образа: -ldflags "-X main.version=<sha>".
var version = "dev"

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.InitTracing(ctx, "notrecinema-api", cfg.OtlpEndpoint)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			logger.Warn("telemetry: ошибка при остановке трейсера", "error", err)
		}
	}()

	metrics := telemetry.NewMetrics(prometheus.DefaultRegisterer)
	telemetry.SetBuildInfo("notrecinema-api", version)

	db, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	// Состояние пула соединений и сводные числа платформы (люди, семьи,
	// сериалы, сессии) считаются в момент скрейпа.
	prometheus.MustRegister(
		telemetry.NewPoolCollector(db.Stat),
		telemetry.NewPlatformStatsCollector(db, logger),
	)

	bus, err := eventbus.Connect(ctx, cfg.NatsURL)
	if err != nil {
		return err
	}
	defer bus.Close()

	poller := outbox.NewPoller(db, bus, logger, metrics)
	go poller.Run(ctx)

	authService := auth.NewService(db, cfg.SessionCookieName, cfg.Environment)
	usersService := users.NewService(db)
	familiesService := families.NewService(db)
	seriesService := series.NewService(db)
	pollsService := polls.NewService(db)
	eventsService := events.NewService(db)
	activityService := activitylog.NewService(db)
	notificationsService := notifications.NewService(db).WithUnsubscribeSecret(cfg.UnsubscribeSecret)
	statsService := stats.NewService(db)
	historyService := history.NewService(db)
	exportService := export.NewService(db)
	calendarService := calendar.NewService(db, cfg.TMDBAPIKey, logger)
	seasonsService := seasons.NewService(db, cfg.KinopoiskAPIKey, cfg.OMDBAPIKey, logger)

	if !calendarService.Configured() {
		logger.Warn("calendar: TMDB_API_KEY не задан, календарь эпизодов отключён")
	}
	go calendarService.RunPeriodicRefresh(ctx, 12*time.Hour)

	if !seasonsService.Configured() {
		logger.Warn("seasons: KINOPOISK_API_KEY/OMDB_API_KEY не заданы, отслеживание сезонов отключено")
	}
	go seasonsService.RunPeriodicRefresh(ctx, 12*time.Hour)

	nudgeChecker := nudges.NewChecker(db, logger, cfg.NudgeCheckInterval, cfg.NudgeDaysThreshold)
	go nudgeChecker.Run(ctx)

	if cfg.DigestEnabled {
		digestWeekday, err := digest.ParseWeekday(cfg.DigestWeekday)
		if err != nil {
			return err
		}
		go digest.NewRunner(db, logger, cfg.DigestCheckInterval, digestWeekday, cfg.DigestHourUTC).Run(ctx)
	}

	uploadService := upload.NewService(upload.Config{
		Endpoint:  cfg.StorageEndpoint,
		Region:    cfg.StorageRegion,
		AccessKey: cfg.StorageAccessKey,
		SecretKey: cfg.StorageSecretKey,
		Bucket:    cfg.StorageBucket,
		PublicURL: cfg.StoragePublicURL,
	}, nil)
	if !uploadService.Configured() {
		logger.Warn("upload: STORAGE_* переменные не заданы, загрузка изображений отключена")
	}

	importsService := imports.NewService(db, cfg.KinopoiskAPIKey, cfg.OMDBAPIKey, cfg.TraktClientID)

	// Источник без ключа просто не появляется в карте -- запрос к нему
	// вернёт понятный 404 вместо падения с ошибкой авторизации у внешнего
	// API при первом же использовании.
	movieProviders := make(map[string]movieprovider.Provider)
	if cfg.TMDBAPIKey != "" {
		movieProviders["tmdb"] = movieprovider.NewResilient(movieprovider.NewTMDB(cfg.TMDBAPIKey, nil), movieprovider.DefaultResilientConfig(), metrics)
	}
	if cfg.OMDBAPIKey != "" {
		movieProviders["omdb"] = movieprovider.NewResilient(movieprovider.NewOMDB(cfg.OMDBAPIKey, nil), movieprovider.DefaultResilientConfig(), metrics)
	}
	if cfg.KinopoiskAPIKey != "" {
		movieProviders["kinopoisk"] = movieprovider.NewResilient(movieprovider.NewKinopoisk(cfg.KinopoiskAPIKey, nil), movieprovider.DefaultResilientConfig(), metrics)
	}

	publicMux := http.NewServeMux()
	publicMux.HandleFunc("GET /healthz", httpserver.Healthz)
	publicMux.HandleFunc("GET /readyz", httpserver.Readyz(db))
	publicMux.Handle("GET /metrics", metrics.Handler())
	auth.RegisterRoutes(publicMux, authService)
	auth.RegisterEmailRoutes(publicMux, authService)
	oauthService := oauth.NewService(authService, cfg.GitHubClientID, cfg.GitHubClientSecret)
	if !oauthService.Configured() {
		logger.Warn("oauth: GITHUB_CLIENT_ID/GITHUB_CLIENT_SECRET не заданы, вход через GitHub отключён")
	}
	oauth.RegisterRoutes(publicMux, oauthService)

	protectedMux := http.NewServeMux()
	auth.RegisterProtectedRoutes(protectedMux, authService)
	auth.RegisterProtectedEmailRoutes(protectedMux, authService)
	auth.RegisterSessionRoutes(protectedMux, authService)
	users.RegisterRoutes(protectedMux, usersService, authService)
	families.RegisterRoutes(protectedMux, familiesService)
	families.RegisterInvitationRoutes(protectedMux, publicMux, familiesService)
	series.RegisterRoutes(protectedMux, seriesService)
	series.RegisterBulkRoutes(protectedMux, seriesService)
	series.RegisterStatusRoutes(protectedMux, seriesService)
	series.RegisterProgressRoutes(protectedMux, seriesService)
	series.RegisterCommentRoutes(protectedMux, seriesService)
	series.RegisterReactionRoutes(protectedMux, seriesService)
	polls.RegisterRoutes(protectedMux, pollsService)
	events.RegisterRoutes(protectedMux, eventsService)
	activitylog.RegisterRoutes(protectedMux, activityService)
	movies.RegisterRoutes(protectedMux, db, movieProviders)
	notifications.RegisterRoutes(protectedMux, notificationsService)
	notifications.RegisterPreferenceRoutes(protectedMux, publicMux, notificationsService)
	stats.RegisterRoutes(protectedMux, statsService)
	history.RegisterRoutes(protectedMux, historyService)
	export.RegisterRoutes(protectedMux, exportService)
	calendar.RegisterRoutes(protectedMux, calendarService)
	seasons.RegisterRoutes(protectedMux, seasonsService)
	upload.RegisterRoutes(protectedMux, uploadService, db)
	imports.RegisterRoutes(protectedMux, importsService)

	// Шаблон маршрута для метрик берётся из r.Pattern, который ServeMux
	// пишет в тот *http.Request, что реально до него дошёл: auth.Middleware
	// делает r.WithContext и создаёт копию, поэтому внешний обработчик
	// выбранного маршрута не увидел бы. Поэтому CaptureRoute оборачивает
	// сами mux'ы (и защищённый, и публичный), а HTTPMiddleware стоит
	// снаружи и считает все запросы, в том числе публичные и отклонённые.
	publicMux.Handle("/", authService.Middleware(telemetry.CaptureRoute(protectedMux)))

	handler := httpserver.Chain(telemetry.CaptureRoute(publicMux),
		metrics.HTTPMiddleware,
		httpserver.RequestID,
		httpserver.AccessLog(logger),
		httpserver.Recover(logger),
		httpserver.CORS(cfg.AllowedOrigins),
		httpserver.Timeout(cfg.RequestTimeout),
	)

	// otelhttp создаёт корневой спан на каждый входящий запрос -- всё
	// остальное (outbox.publish, обработка в notrecinema-worker) станет
	// дочерними спанами этого же trace через контекст запроса.
	handler = otelhttp.NewHandler(handler, "notrecinema-api")

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("http server starting", "port", cfg.Port)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	select {
	case err := <-serverErrors:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}

	logger.Info("shutdown complete")
	return nil
}
