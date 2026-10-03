// Package config загружает и валидирует конфигурацию процесса из
// переменных окружения через cleanenv. Ошибка при старте (например,
// не задан обязательный DATABASE_URL) — лучше, чем непонятный сбой
// в рантайме из-за отсутствующей переменной.
package config

import (
	"fmt"
	"time"

	"github.com/ilyakaznacheev/cleanenv"
)

// Config — вся конфигурация процесса. Теги env/env-default/env-required
// читает cleanenv: он же обеспечивает fail-fast при старте, если
// обязательное поле не задано.
type Config struct {
	Port string `env:"PORT" env-default:"8080"`

	// DatabaseURL аутентифицируется под RLS-ролью app_user, а не под
	// суперпользователем. Специально совпадает по смыслу с DATABASE_URL
	// в Next.js-приложении: оба сервиса используют один и тот же Postgres
	// и один и тот же набор RLS-политик на время переезда с монолита.
	DatabaseURL string `env:"DATABASE_URL" env-required:"true"`

	// NatsURL -- адрес брокера JetStream, куда outbox-поллер публикует
	// накопленные события.
	NatsURL string `env:"NATS_URL" env-default:"nats://localhost:4222"`

	// OtlpEndpoint -- адрес коллектора трейсов (Jaeger, слушает OTLP/HTTP).
	OtlpEndpoint string `env:"OTLP_ENDPOINT" env-default:"localhost:4318"`

	// AllowedOrigins — allow-list для CORS, через запятую.
	AllowedOrigins []string `env:"ALLOWED_ORIGINS" env-separator:","`

	// SessionCookieName должен совпадать с именем cookie в Next.js-приложении,
	// иначе оба сервиса не будут узнавать одну и ту же сессию входа.
	SessionCookieName string `env:"SESSION_COOKIE_NAME" env-default:"notre_cinema_session"`

	ShutdownTimeout   time.Duration `env:"SHUTDOWN_TIMEOUT" env-default:"15s"`
	ReadHeaderTimeout time.Duration `env:"READ_HEADER_TIMEOUT" env-default:"5s"`
	RequestTimeout    time.Duration `env:"REQUEST_TIMEOUT" env-default:"10s"`

	// Ключи внешних API метаданных фильмов -- все опциональны. Источник
	// без ключа просто не регистрируется в internal/movies (см. main.go),
	// а не падает при первом запросе.
	TMDBAPIKey      string `env:"TMDB_API_KEY"`
	OMDBAPIKey      string `env:"OMDB_API_KEY"`
	KinopoiskAPIKey string `env:"KINOPOISK_API_KEY"`

	// NudgeCheckInterval -- как часто internal/nudges ищет заброшенные
	// сериалы. Раз в сутки достаточно: сама staleness-проверка (14 дней) и
	// недельный троттлинг повторных напоминаний на стороне SQL всё равно не
	// изменятся быстрее.
	NudgeCheckInterval time.Duration `env:"NUDGE_CHECK_INTERVAL" env-default:"24h"`
	NudgeDaysThreshold int           `env:"NUDGE_DAYS_THRESHOLD" env-default:"14"`

	// Еженедельная сводка семьи (internal/digest): включена по умолчанию.
	// Рассылка срабатывает один раз в неделю в DigestWeekday не раньше
	// DigestHourUTC (по умолчанию понедельник 06:00 UTC -- 09:00 в Москве);
	// проверка идёт каждые DigestCheckInterval.
	DigestEnabled       bool          `env:"DIGEST_ENABLED" env-default:"true"`
	DigestWeekday       string        `env:"DIGEST_WEEKDAY" env-default:"monday"`
	DigestHourUTC       int           `env:"DIGEST_HOUR_UTC" env-default:"6"`
	DigestCheckInterval time.Duration `env:"DIGEST_CHECK_INTERVAL" env-default:"30m"`

	// UnsubscribeSecret -- HMAC-ключ ссылок отписки в письмах. Общий с
	// notrecinema-worker: воркер подписывает ссылку при отправке, API
	// проверяет. Без него отписка по ссылке не работает (запрос завершается ошибкой).
	UnsubscribeSecret string `env:"UNSUBSCRIBE_SECRET"`

	// Environment -- "production" включает Secure на cookie сессии/OAuth-state
	// (тот же process.env.NODE_ENV === 'production' чек, что в notrecinema-app).
	Environment string `env:"APP_ENV" env-default:"development"`

	// GitHub OAuth -- опционально, как и остальные внешние интеграции: без
	// ключей POST /api/v1/auth/github/start отвечает понятной ошибкой вместо
	// падения при первом клике.
	GitHubClientID     string `env:"GITHUB_CLIENT_ID"`
	GitHubClientSecret string `env:"GITHUB_CLIENT_SECRET"`

	// Kinopoisk/OMDB уже опциональны выше; TraktClientID -- ещё один источник
	// импорта, той же опциональной формы.
	TraktClientID string `env:"TRAKT_CLIENT_ID"`

	// Object storage (S3-совместимое, включая MinIO) для загрузки постера
	// сериала -- опционально, как и остальные внешние интеграции.
	StorageEndpoint  string `env:"STORAGE_ENDPOINT"`
	StorageRegion    string `env:"STORAGE_REGION"`
	StorageAccessKey string `env:"STORAGE_ACCESS_KEY"`
	StorageSecretKey string `env:"STORAGE_SECRET_KEY"`
	StorageBucket    string `env:"STORAGE_BUCKET"`
	StoragePublicURL string `env:"STORAGE_PUBLIC_URL"`
}

func Load() (Config, error) {
	var cfg Config
	if err := cleanenv.ReadEnv(&cfg); err != nil {
		return Config{}, err
	}
	if cfg.DigestHourUTC < 0 || cfg.DigestHourUTC > 23 {
		return Config{}, fmt.Errorf("config: DIGEST_HOUR_UTC должен быть от 0 до 23, получено %d", cfg.DigestHourUTC)
	}
	return cfg, nil
}
