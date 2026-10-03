# Метрики

API и воркер отдают Prometheus-метрики на `GET /metrics`: API — на порту
сервиса (`PORT`, по умолчанию 8080), воркер — на `:8081`. Наружу они не
выставляются: роль nginx в `notrecinema-infra` по умолчанию отвечает 404 на
`/metrics` у каждого сайта (`nginx_default_deny_paths`), Prometheus ходит к
сервисам напрямую по внутренней сети.

Сбор — VictoriaMetrics, дашборды — Grafana, правила алертов — vmalert; всё
это разворачивает роль `monitoring` из `notrecinema-infra` (README там же).
Заметьте: у метрик фоновых задач есть собственная метка `job` (`nudges`,
`seasons_refresh`, `calendar_refresh`), а сборщик сам присваивает метрике
`job` с именем scrape-задачи, поэтому в запросах метка задачи называется
`exported_job`.

Имена и метки ниже взяты из кода (`internal/telemetry`, `internal/outbox`,
`notrecinema-worker/internal/telemetry`). Помимо перечисленного, процессы
отдают стандартные `go_*` и `process_*`.

## API

### HTTP

| Метрика | Тип | Метки | Что показывает |
| --- | --- | --- | --- |
| `http_requests_total` | counter | `method`, `path`, `status` | Запросы. `path` — шаблон маршрута (`/api/v1/series/{seriesId}`), не реальный URL; несопоставленные пути — `unmatched`. `/metrics` не считается. |
| `http_request_duration_seconds` | histogram | `method`, `path` | Длительность обработки. |
| `http_response_size_bytes` | histogram | `method`, `path` | Размер ответа. |
| `http_requests_in_flight` | gauge | — | Сколько запросов обрабатывается сейчас. |
| `http_panics_total` | counter | — | Паники в хендлерах, перехваченные `Recover`. Любое значение > 0 — баг. |

### Аутентификация и лимиты

| Метрика | Тип | Метки | Что показывает |
| --- | --- | --- | --- |
| `auth_events_total` | counter | `event`, `result` | Вход, регистрация, 2FA, резервные коды, сброс и смена пароля, сессии, отписка. Примеры: `login/invalid_credentials`, `login/two_factor_required`, `register/duplicate`, `two_factor_verify/expired_challenge`, `password_change/wrong_password`, `session_revoke/others`, `backup_codes_regenerate/invalid_code`. |
| `rate_limit_checks_total` | counter | `bucket`, `result` | Проверки лимитов; `result` — `allowed` или `rejected`. |

### База данных

| Метрика | Тип | Метки | Что показывает |
| --- | --- | --- | --- |
| `db_queries_total` | counter | `operation`, `result` | Запросы: `select`/`insert`/`update`/`delete`/`function`/`other`; `ok`/`error`/`canceled`. |
| `db_query_duration_seconds` | histogram | `operation` | Длительность запроса. |
| `db_transactions_total` | counter | `result` | Транзакции под пользовательским контекстом: `commit`/`rollback`. |
| `db_transaction_duration_seconds` | histogram | — | Длительность транзакции целиком. |
| `db_pool_connections` | gauge | `state` | Соединения пула по состоянию (`acquired`, `idle`, `constructing`). |
| `db_pool_max_connections` | gauge | — | Размер пула. |
| `db_pool_acquires_total` | counter | — | Взятия соединения из пула. |
| `db_pool_empty_acquires_total` | counter | — | Взятия, когда свободных соединений не было (пришлось ждать). |
| `db_pool_canceled_acquires_total` | counter | — | Ожидания, прерванные отменой контекста. |
| `db_pool_acquire_wait_seconds_total` | counter | — | Суммарное время ожидания соединения. |
| `db_pool_new_connections_total` | counter | — | Новые соединения. |
| `db_pool_destroyed_connections_total` | counter | `reason` | Закрытые соединения (`max_lifetime`, `max_idle`). |

### Outbox (доставка событий в NATS)

| Метрика | Тип | Метки | Что показывает |
| --- | --- | --- | --- |
| `outbox_pending` | gauge | — | Сколько событий ещё не опубликовано. |
| `outbox_oldest_pending_age_seconds` | gauge | — | Возраст самого старого неопубликованного события; растёт, если публикация зависла. |
| `outbox_published_total` | counter | — | Опубликовано событий. |
| `outbox_events_published_total` | counter | `event_type` | То же по типу события. |
| `outbox_failed_total` | counter | — | Неудачные попытки публикации. |
| `outbox_publish_duration_seconds` | histogram | — | Длительность публикации одного события. |

### Внешние сервисы и фоновые задачи

| Метрика | Тип | Метки | Что показывает |
| --- | --- | --- | --- |
| `outbound_http_requests_total` | counter | `host`, `outcome` | Исходящие запросы по хосту; `outcome` — `success`/`client_error`/`server_error`/`error`. |
| `outbound_http_request_duration_seconds` | histogram | `host` | Длительность исходящего запроса. |
| `external_api_requests_total` | counter | `provider`, `outcome` | Запросы к API метаданных фильмов (TMDB, OMDb, Кинопоиск). |
| `external_api_duration_seconds` | histogram | `provider` | Длительность таких запросов. |
| `background_job_runs_total` | counter | `job`, `result` | Запуски задач `nudges`, `seasons_refresh`, `calendar_refresh`. |
| `background_job_duration_seconds` | histogram | `job` | Длительность запуска. |
| `background_job_last_success_timestamp_seconds` | gauge | `job` | Когда задача в последний раз завершилась успешно. |

### Платформа и сборка

| Метрика | Тип | Метки | Что показывает |
| --- | --- | --- | --- |
| `platform_users` | gauge | `state` | Пользователи: `all`, `verified` (email подтверждён), `two_factor` (2FA включена). |
| `platform_families`, `platform_series` | gauge | — | Семьи и сериалы. |
| `platform_active_sessions` | gauge | — | Действующие сессии. |
| `platform_push_subscriptions` | gauge | — | Подписки на push. |
| `platform_pending_invitations` | gauge | — | Неиспользованные приглашения в семью. |
| `platform_stats_errors_total` | counter | — | Сбои чтения этих чисел (значения кэшируются на 30 с). |
| `build_info` | gauge | `service`, `version`, `go_version` | Всегда 1; версия — хеш коммита (`ARG VERSION` в Dockerfile). |

## Воркер

| Метрика | Тип | Метки | Что показывает |
| --- | --- | --- | --- |
| `worker_jobs_total` | counter | `event_type`, `outcome` | Обработанные события: `success`, `failure`, `duplicate`, `dead_letter`, `no_handler`, `error`. |
| `worker_job_duration_seconds` | histogram | `event_type` | Длительность обработчика. |
| `worker_event_age_seconds` | histogram | `event_type` | От записи события в outbox до начала обработки: сквозная задержка API → NATS → воркер. |
| `worker_events_in_flight` | gauge | — | Событий в обработке сейчас. |
| `worker_event_redeliveries_total` | counter | `event_type` | Повторные доставки после ошибки. |
| `worker_dead_letter_published_total` | counter | `event_type`, `result` | Публикации в dead-letter стрим (`ok`/`error`). |
| `worker_dead_letter_messages` | gauge | — | Сообщений в dead-letter стриме. Любое значение > 0 — повод разобраться. |
| `worker_consumer_pending_messages` | gauge | — | Очередь в NATS, ещё не доставленная консьюмеру. |
| `worker_consumer_ack_pending_messages` | gauge | — | Доставлено, но не подтверждено. |
| `worker_consumer_redelivered_messages` | gauge | — | Ждут повторной доставки. |
| `worker_nats_connected` | gauge | — | 1, если воркер подключён к NATS. |
| `worker_nats_reconnects_total` | counter | — | Переподключения к NATS. |
| `worker_mail_total` | counter | `kind`, `result` | Письма по виду (`verify_email`, `reset_password`, …) и результату `sent`/`failed`. |
| `worker_mail_send_duration_seconds` | histogram | `kind` | Длительность отправки через Resend. |
| `worker_mail_skipped_total` | counter | `kind`, `reason` | Письма, не отправленные намеренно (`disabled`, `unverified`, `opted_out`, …). |
| `worker_push_total` | counter | `category`, `result` | Web push: `sent`/`dead_subscription`/`failed`. |
| `worker_push_send_duration_seconds` | histogram | — | Длительность отправки push. |
| `worker_tokens_created_total` | counter | `kind` | Одноразовые токены, выпущенные воркером. |
| `outbound_http_requests_total`, `outbound_http_request_duration_seconds` | | `host` (, `outcome`) | Исходящие запросы воркера (Resend, push-сервисы браузеров). |
| `build_info` | gauge | `service`, `version`, `go_version` | Версия воркера. |

## Предлагаемые алерты

Пороги — отправная точка, подбирайте по реальному трафику.

```yaml
groups:
  - name: notrecinema
    rules:
      - alert: ApiDown
        expr: up{job="notrecinema-api"} == 0
        for: 2m

      - alert: ApiHighErrorRate
        expr: |
          sum(rate(http_requests_total{status=~"5.."}[5m]))
            / sum(rate(http_requests_total[5m])) > 0.02
        for: 10m

      - alert: ApiSlow
        expr: |
          histogram_quantile(0.95,
            sum by (le) (rate(http_request_duration_seconds_bucket[5m]))) > 1
        for: 10m

      - alert: ApiPanics
        expr: increase(http_panics_total[10m]) > 0

      - alert: DbPoolStarved
        expr: |
          rate(db_pool_empty_acquires_total[5m])
            / rate(db_pool_acquires_total[5m]) > 0.2
        for: 10m

      - alert: DbQueryErrors
        expr: sum(rate(db_queries_total{result="error"}[5m])) > 0.1
        for: 10m

      # События копятся в outbox: публикация в NATS стоит.
      - alert: OutboxStuck
        expr: outbox_oldest_pending_age_seconds > 120
        for: 5m

      # Воркер отстаёт: письма и push приходят с задержкой.
      - alert: WorkerLagging
        expr: |
          histogram_quantile(0.95,
            sum by (le) (rate(worker_event_age_seconds_bucket[5m]))) > 60
        for: 10m

      - alert: WorkerDown
        expr: worker_nats_connected == 0
        for: 3m

      - alert: DeadLetterNotEmpty
        expr: worker_dead_letter_messages > 0

      - alert: MailFailing
        expr: |
          sum(rate(worker_mail_total{result="failed"}[15m]))
            / sum(rate(worker_mail_total[15m])) > 0.1
        for: 15m

      # Фоновая задача давно не отрабатывала успешно.
      - alert: BackgroundJobStale
        expr: |
          time() - background_job_last_success_timestamp_seconds
            > 3 * 86400

      # Всплеск неудачных входов: подбор паролей.
      - alert: LoginBruteForce
        expr: |
          sum(rate(auth_events_total{event="login",result="invalid_credentials"}[5m])) > 1
        for: 10m

      - alert: RateLimitSpike
        expr: sum(rate(rate_limit_checks_total{result="rejected"}[5m])) > 5
        for: 10m
```

Что стоит вынести на дашборды: RPS и p95 по маршрутам (`path`), доля 5xx,
пул соединений БД, очередь и возраст outbox, задержка воркера
(`worker_event_age_seconds`), отправленные/неудачные письма по `kind`,
события аутентификации по `event`/`result`, числа `platform_*`.
