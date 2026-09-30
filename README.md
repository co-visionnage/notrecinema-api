# notrecinema-api

Go backend, вынесенный из Next.js-монолита notrecinema-app. Шаг 1 из плана
в [docs/ROADMAP.md](docs/ROADMAP.md): обычный HTTP API перед той же базой
PostgreSQL, использующий cookie сессии монолита, чтобы оба сервиса
одинаково узнавали залогиненного пользователя на время переезда.

## Почему это устроено именно так

- **Без фреймворка.** `net/http.ServeMux` в Go 1.22+ уже умеет роутинг по
  методу и path-паттерну (`GET /api/v1/users/me`), так что тащить
  сторонний роутер незачем.
- **Row-level security живёт в Postgres, а не в Go.** Каждый запрос,
  трогающий пользовательские данные, идёт через
  `postgres.Pool.WithUserContext`, который устанавливает
  `app.current_user_id` на транзакцию — тот же механизм, что и
  `withUserContext` в notrecinema-app
  (`src/shared/api/postgres/database.ts`). Оба сервиса применяют одинаковые
  правила доступа, потому что оба полагаются на одни и те же RLS-политики,
  а не переизобретают авторизацию каждый по-своему.
- **Сессии выпускает и этот сервис, и notrecinema-app — одним и тем же
  способом.** `internal/auth` (логин/регистрация/логаут/2FA) и
  `internal/oauth` (GitHub) используют те же SQL-функции
  (`create_session_for_profile`, `register_profile_account`,
  `get_session_user`, ...) и тот же формат хэша пароля (scrypt,
  `"<соль>:<ключ>"`, см. `internal/auth/password.go`), что и
  `src/shared/api/postgres/auth.ts` в notrecinema-app. Один и тот же
  пользователь может залогиниться через любой из двух сервисов и получить
  сессию, которую оба одинаково узнают — общий формат, а не общий эмитент.
- **Схема БД не дублируется.** Она вынесена в отдельный пакет
  [`../notrecinema-schema`](../notrecinema-schema) — единственный источник
  истины, которым пользуются notrecinema-app, notrecinema-api и
  notrecinema-worker.
- **События публикуются атомарно с данными (outbox), а не отдельным
  вызовом.** `internal/outbox.Insert` кладёт событие в `outbox_events`
  внутри той же транзакции, что и основная запись — событие никогда не
  теряется и никогда не публикуется "из воздуха" при откате.

## Структура

```
cmd/api/                entrypoint: сборка зависимостей, graceful shutdown
internal/config/        конфигурация из env через cleanenv, fail-fast
internal/logging/       структурированное JSON-логирование (slog)
internal/postgres/      pgx pool + RLS-транзакции
internal/httpserver/    middleware (request ID, access log, recover, CORS, timeout), health-проверки
internal/auth/          логин/регистрация/логаут/2FA -- выпуск и проверка cookie сессии
internal/oauth/         вход через GitHub (привязка по email)
internal/upload/        загрузка постера сериала в S3-совместимое хранилище
internal/imports/       поиск (Kinopoisk/OMDb), импорт Trakt watchlist, разбор IMDb CSV
internal/users/         вертикальный срез "профиль" + удаление аккаунта
internal/families/      "семьи": создание/вступление/участники (список, роли, передача владения)
internal/series/        сериалы семьи: сам сериал, статус просмотра, прогресс по эпизодам, комментарии, реакции
internal/polls/         опросы "что смотрим сегодня" (создание, голосование, закрытие)
internal/events/        запланированные совместные просмотры + RSVP
internal/activitylog/   журнал активности семьи (кто что добавил/закрыл)
internal/movies/        HTTP-обёртка над movieprovider: GET /api/v1/movies/{source}/{id}
internal/notifications/ управление push-подписками (сама отправка -- в notrecinema-worker)
internal/stats/         статистика, достижения, итоги года, рекомендации
internal/history/       журнал просмотров семьи (кто что посмотрел и когда)
internal/export/        выгрузка собственных данных пользователя (JSON/CSV)
internal/calendar/      дата следующего эпизода (TMDB) для сериалов из OMDb/IMDb-CSV
internal/seasons/       отслеживание новых сезонов (Kinopoisk/OMDb) + уведомление семьи
internal/nudges/        напоминания о заброшенных сериалах (фоновая проверка + outbox)
internal/outbox/        transactional outbox: Insert + фоновый Poller
internal/eventbus/      обёртка над NATS JetStream (публикация, проброс trace-контекста)
internal/movieprovider/ MovieProvider: TMDB/OMDB/Kinopoisk + resilience (timeout/retry/circuit breaker/fallback)
internal/telemetry/     метрики (Prometheus) и трейсинг (OpenTelemetry -> Jaeger)
internal/platform/      apperror (доменные ошибки -> HTTP-статус), response (JSON-конверт)
```

Каждый доменный пакет (`users`, `families`, ...) владеет и сервисной
логикой, и HTTP-хендлерами — та же структура "модули, а не слои", что и в
`notrecinema-app/src`.

## Запуск локально

```bash
cp .env.example .env
docker compose up --build
```

Поднимает всю backend-платформу разом: Postgres, `migrate` (применяет все
миграции из `../notrecinema-schema`), NATS (JetStream), API на
`localhost:8090` (маппится с 8080 внутри контейнера — поменяйте порт в
`docker-compose.yml`, если 8090 занят) и `notrecinema-worker` (собирается
из `../notrecinema-worker`, потребляет события, которые API публикует
через outbox).

Health-проверки:

```bash
curl localhost:8090/healthz   # liveness -- всегда OK, если процесс жив
curl localhost:8090/readyz    # readiness -- падает, если Postgres недоступен
```

Все остальные роуты требуют ту же cookie сессии (`notre_cinema_session` по
умолчанию) -- выставленную либо этим сервисом, либо notrecinema-app: оба
используют один и тот же формат.

## API

```
POST   /api/v1/auth/login            -- {mode: login|register, email, password, ...}
POST   /api/v1/auth/logout
POST   /api/v1/auth/verify-2fa       -- {challengeToken, code} -- второй шаг логина с включённой 2FA
POST   /api/v1/auth/2fa/setup        -- возвращает {secret, qrCodeDataUrl}
POST   /api/v1/auth/2fa/confirm      -- {code}
POST   /api/v1/auth/2fa/disable      -- {code}
GET    /api/v1/auth/2fa/status       -- {enabled}
GET    /api/v1/auth/github/start     -- редирект на GitHub (требует ?legalAccepted=true)
GET    /api/v1/auth/github/callback

GET    /api/v1/users/me
DELETE /api/v1/users/me              -- {password} -- удаление аккаунта, блокируется, если владеешь семьёй с другими участниками

GET    /api/v1/families
POST   /api/v1/families
POST   /api/v1/families/join                    -- {inviteCode}
GET    /api/v1/families/{familyId}/members
DELETE /api/v1/families/{familyId}/members/{userId}       -- только владелец, владельца удалить нельзя
PUT    /api/v1/families/{familyId}/members/{userId}/role  -- {role: admin|member}, только владелец
POST   /api/v1/families/{familyId}/transfer-ownership     -- {newOwnerUserId}

GET    /api/v1/families/{familyId}/series
POST   /api/v1/families/{familyId}/series
POST   /api/v1/families/{familyId}/series/bulk   -- {items[]} -- массовый импорт (до 500), одно агрегированное уведомление
GET    /api/v1/series/{seriesId}
PATCH  /api/v1/series/{seriesId}
DELETE /api/v1/series/{seriesId}

PUT    /api/v1/series/{seriesId}/status          -- watched/to-watch, rating, comment (upsert)
GET    /api/v1/series/{seriesId}/status
DELETE /api/v1/series/{seriesId}/status

PUT    /api/v1/series/{seriesId}/progress        -- currentSeason/currentEpisode (upsert)
GET    /api/v1/series/{seriesId}/progress

POST   /api/v1/series/{seriesId}/comments
GET    /api/v1/series/{seriesId}/comments
DELETE /api/v1/comments/{commentId}

POST   /api/v1/series/{seriesId}/reactions       -- {emoji} (идемпотентно)
GET    /api/v1/series/{seriesId}/reactions
DELETE /api/v1/series/{seriesId}/reactions/{emoji}

POST   /api/v1/families/{familyId}/polls         -- {title, seriesIds[]}
GET    /api/v1/families/{familyId}/polls         -- с подсчётом голосов по опциям
POST   /api/v1/polls/{pollId}/votes              -- {optionId} (upsert собственного голоса)
POST   /api/v1/polls/{pollId}/close              -- только автор опроса или владелец семьи

POST   /api/v1/families/{familyId}/events        -- {title, scheduledAt, seriesId?}
GET    /api/v1/families/{familyId}/events        -- с RSVP каждого участника
PUT    /api/v1/events/{eventId}/rsvp             -- {status: going|maybe|no}
DELETE /api/v1/events/{eventId}                  -- только автор события или владелец семьи

GET    /api/v1/families/{familyId}/activity      -- журнал активности семьи

GET    /api/v1/movies/{source}/{externalId}      -- source: tmdb|omdb|kinopoisk (только сконфигурированные, rate-limited 60/10мин)

POST   /api/v1/notifications/subscriptions       -- {endpoint, keys:{p256dh, auth}} (upsert по endpoint)
DELETE /api/v1/notifications/subscriptions       -- {endpoint}

GET    /api/v1/families/{familyId}/stats
GET    /api/v1/families/{familyId}/achievements
GET    /api/v1/families/{familyId}/wrapped?year=2026
GET    /api/v1/families/{familyId}/recommendations

GET    /api/v1/families/{familyId}/history          -- журнал просмотров, ?offset= для пагинации
GET    /api/v1/export/data?format=json|csv          -- выгрузка собственных данных (профиль, семья, сериалы)

GET    /api/v1/families/{familyId}/calendar          -- {upcoming, checkable}
POST   /api/v1/series/{seriesId}/next-episode/check  -- обновить дату следующего эпизода (rate-limited, требует TMDB_API_KEY)

POST   /api/v1/series/{seriesId}/season-updates/check  -- проверить новый сезон (rate-limited, требует KINOPOISK_API_KEY/OMDB_API_KEY)

GET    /api/v1/import/search?query=      -- Kinopoisk+OMDb, требует хотя бы один из ключей
GET    /api/v1/import/trakt?username=    -- публичный watchlist, требует TRAKT_CLIENT_ID
POST   /api/v1/import/csv                -- {csv} -- разбор экспорта IMDb, без сети

POST   /api/v1/upload/image              -- multipart/form-data, поле file, до 5 МБ, требует STORAGE_*
```

Права нигде не проверяются дважды: Go-слой доверяет RLS-политикам из
`notrecinema-schema` (членство в семье, авторство, владение) и просто
транслирует их отказ в 403/404 -- см. комментарии в исходниках
`internal/series`, `internal/polls`, `internal/events` с указанием, какая
именно миграция отвечает за какое правило.

## Observability

```bash
curl localhost:8090/metrics     # Prometheus-метрики API
curl localhost:8091/metrics     # Prometheus-метрики воркера
open http://localhost:16686     # Jaeger UI -- трейсы HTTP -> outbox -> NATS -> worker
```

Один HTTP-запрос, породивший событие (например `POST /api/v1/families`),
и последующая асинхронная публикация в outbox -- разные trace'ы: сама
HTTP-транзакция обычно успевает завершиться и экспортировать спан раньше,
чем поллер (интервал 2с) её подхватит. Начиная с `outbox.publish`
(в `notrecinema-api`) и до `worker.process` (в `notrecinema-worker`) --
один общий trace, проверено вживую через Jaeger API.

## CI/CD

`.gitlab-ci.yml`: `go vet` + `golangci-lint`, юнит- и интеграционные тесты
(последние -- против эфемерных Postgres+NATS в CI, со схемой из
`notrecinema-schema`, клонируемой по переменной `SCHEMA_REPO_URL`),
Trivy-сканы, сборка образа через Kaniko, ручной SSH-деплой. Задайте
`SCHEMA_REPO_URL`, `SSH_PRIVATE_KEY`, `SSH_KNOWN_HOSTS`, `SSH_USER`,
`SSH_HOST`, `DEPLOY_PATH` как CI/CD-переменные проекта.

## Тесты

```bash
go test ./...                                       # юнит-тесты, без внешних зависимостей
DATABASE_URL=... go test -tags integration ./...    # против настоящего Postgres
```

Интеграционные тесты создают реальных пользователей через общую
SQL-функцию `create_profile_session` и проверяют, что RLS действительно
изолирует их друг от друга — не просто что запросы отрабатывают без ошибок.

`internal/movieprovider` тестируется отдельно: намеренно нестабильный
`chaosProvider` проверяет, что ретраи, обрыв по таймауту и открытие circuit
breaker'а реально происходят, а не только компилируются.

`internal/series`, `internal/polls`, `internal/events` проверяют не только
happy path, но и то, что RLS реально отклоняет: голос за опцию из чужого
опроса, закрытие опроса не-автором, удаление события не-автором, сериал
чужой семьи, невидимый через `ListForFamily`.

`internal/stats` проверяется реальными числами, а не просто отсутствием
ошибки: 10 эпизодов × 60 минут действительно дают 10 часов, `wrapped`
действительно фильтрует по году, рекомендации действительно ранжируются
по жанрам, которые семья уже любит (rating >= 4), и не включают уже
просмотренное.

Push-уведомления протестированы настоящей криптографией: unit-тесты в
`notrecinema-worker/internal/webpush` гоняют реальный VAPID JWT + aes128gcm
(RFC8291) через httptest, а end-to-end сценарий (два пользователя, join по
инвайт-коду, подписка, добавление сериала) реально доставил зашифрованный
push стороннему HTTP-серверу через Docker-сеть -- не просто "функция
вызвалась без ошибки".

`internal/history` проверен на то, ради чего и понадобилась миграция 0008:
один участник семьи реально видит "watched"-отметку другого, а не только
свою. `internal/export` проверен реальными значениями (профиль, семья,
сериал с рейтингом/комментарием/прогрессом) через настоящий HTTP —
и JSON, и CSV. `internal/nudges` проверен так: сериал с прогрессом,
искусственно состаренным на 20 дней, реально получает ровно одно
`progress.stale`-событие в outbox, а повторный прогон в тот же день его не
дублирует (недельный троттлинг на стороне `mark_progress_reminded`) —
свежий прогресс (2 дня) событие не получает вообще. `internal/calendar` и
`internal/seasons` проверены без реального обращения к внешним API (URL
захардкожены не ради теста): RLS реально прячет чужой сериал (404), а
сериал без привязки к внешнему каталогу реально отклоняется (400) — то
самое место, о котором явно предупреждает комментарий в миграции 0018.
Rate-limit на `GET /api/v1/movies/...` проверен реально срабатывающим:
61-й запрос от одного пользователя за 10 минут получает 429, а не молча
проходит. Dead-letter стрим и удаление мёртвых push-подписок проверены
через `docker compose`: стрим `NOTRECINEMA_DEADLETTER` реально создаётся
при старте воркера, виден в `nats-server` мониторинге (`:8222`).

`internal/auth` проверен полным жизненным циклом против настоящего
Postgres: регистрация → логин → 2FA setup/confirm (настоящий TOTP-код,
`pquerna/otp`, те же дефолты, что otplib: 6 цифр/30с/SHA1/±1 шаг) →
повторный логин реально требует код → verify-2fa выдаёт сессию → disable →
logout реально удаляет строку `app_sessions`. Плюс сквозная проверка через
реальный HTTP в `docker compose`: тот же цикл целиком, включая
`DELETE /api/v1/users/me` (удаление аккаунта) в конце. По пути найдена и
исправлена настоящая утечка соединений пула: `Logout`/`Login`/
`VerifyTwoFactor` вызывали `db.Query` и не читали и не закрывали
результат -- pgx не освобождает соединение обратно в пул, пока `Rows` не
исчерпаны или не закрыты явно; на серии таких вызовов пул (20 соединений)
исчерпывался и всё, что дальше просило соединение, зависало без ошибки, а
не падало с понятным таймаутом. Добавлен `postgres.Pool.Exec` — обёртка
над `pgx.Exec`, которая не может забыть закрыть за собой `Rows`, потому
что их вообще не возвращает.

`internal/users.DeleteAccount` проверен так: неверный пароль отклоняется;
владелец семьи с другими участниками реально блокируется (409), профиль
остаётся на месте (проверено через его же RLS-контекст — `db.QueryRow` без
`WithUserContext` здесь ничего не докажет, `profiles_select_self` без
`current_user_id()` всегда возвращает "не найдено", независимо от
реального состояния строки); одиночный пользователь реально удаляется,
и это подтверждено чёрным ящиком — повторный логин с теми же
учётными данными больше не проходит (по той же причине RLS-прозрачности
нельзя было просто проверить исчезновение строки напрямую).

`internal/families` (участники): `ListMembers` реально возвращает
владельца первым, `RemoveMember` реально запрещён не-владельцу и на
самого владельца, `TransferOwnership` реально переставляет роли. По пути
найден и исправлен настоящий баг схемы: `family_members` не имел ни одной
UPDATE-политики RLS вообще — оригинальный `setMemberRoleAction` в
notrecinema-app делал обычный `UPDATE` под RLS, который поэтому
гарантированно не работал ни при каких правах вызывающего (0 затронутых
строк, без ошибки). Добавлена SQL-функция `set_family_member_role`
(SECURITY DEFINER, миграция 0030), которая реально меняет роль.

`internal/series.CreateBulk` (массовый импорт) проверен на двух отдельных
`INSERT`, как и в оригинале (не через один `WITH`-запрос — RLS-политика
`family_series_status_insert_owner` не видит строки, вставленные sibling'ом
в том же WITH, см. комментарий в коде): статус/рейтинг/комментарий реально
привязываются к своим сериалам через `matchBulkImportedRowsToItems`, лимит
500 элементов реально отклоняется, пустой список — не-операция без ошибки.

`internal/imports` (IMDb CSV) проверен без сети — настоящим RFC4180-парсером
с кавычками, запятыми и экранированными кавычками внутри полей,
регистронезависимым поиском колонок по имени (не по позиции), пропуском
строк без `Const`/`Title`.

`internal/upload` (S3) и `internal/oauth` (GitHub) проверены дважды: сперва
unit-тестами (реальная SigV4-подпись против httptest-сервера — не мок
функции подписи, а настоящий подписанный PUT с проверкой заголовков; отказ
OAuth без `legalAccepted`/конфигурации/с неверным `state`), а затем живьём
против настоящих внешних сервисов. Поднят реальный MinIO (образ
`elestio/minio` — официальный `minio/minio` больше не тянется с Docker Hub
без логина), через настоящий `POST /api/v1/upload/image` загружен
настоящий PNG, скачан обратно по публичному URL — байты идентичны
(`cmp` подтвердил). Зарегистрирован настоящий OAuth App на github.com и
пройден полный живой consent-флоу в браузере: `github/start` →
авторизация на GitHub → callback → обмен кода на токен →
`/user`+`/user/emails` → `GET /api/v1/users/me` в том же браузере вернул
настоящие email/имя с GitHub, `password_hash IS NULL` в БД (OAuth-only
аккаунт). CSRF (неверный `state`) отклоняется вживую, не только в тестах.

## Что дальше

См. [docs/ROADMAP.md](docs/ROADMAP.md). Сознательно отложено: Redis (когда
появится реальный медленный вызов, который нужно кэшировать) и
email-доставка в `internal/nudges` (в Go-сервисах нет SMTP-инфраструктуры,
есть только push) и в `internal/seasons` (тот же case).
