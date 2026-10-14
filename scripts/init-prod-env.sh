#!/usr/bin/env bash
# Создаёт .env для docker-compose.prod.yml на сервере: случайные пароли БД и
# секрет ссылок отписки, прод-значения адресов и хранилища.
# Секреты на экран не выводятся. Ключи внешних сервисов (Resend, VAPID,
# GitHub OAuth, TMDB/OMDb/Кинопоиск) остаются пустыми -- впишите их в .env
# сами: без них соответствующие функции выключены, остальное работает.
#
# Запуск на сервере под пользователем deploy (из репозитория):
#   cd /opt/notrecinema && bash scripts/init-prod-env.sh
# или из любого места: DEPLOY_DIR=/opt/notrecinema bash init-prod-env.sh
set -euo pipefail

cd "${DEPLOY_DIR:-$(dirname "$0")/..}"

if [ -e .env ]; then
  echo ".env уже существует, ничего не меняю." >&2
  exit 1
fi
command -v openssl >/dev/null || { echo "нужен openssl" >&2; exit 1; }

domain="${APP_DOMAIN:-notrecinema.ru}"
postgres_password="$(openssl rand -hex 24)"
app_db_password="$(openssl rand -hex 24)"
storage_access_key="$(openssl rand -hex 12)"
storage_secret_key="$(openssl rand -hex 24)"
unsubscribe_secret="$(openssl rand -hex 32)"

umask 077
cat >.env <<ENV
POSTGRES_DB=notrecinema
POSTGRES_USER=postgres
POSTGRES_PASSWORD=${postgres_password}
APP_DB_PASSWORD=${app_db_password}
DATABASE_URL=postgresql://app_user:${app_db_password}@postgres:5432/notrecinema
MIGRATE_DATABASE_URL=postgresql://postgres:${postgres_password}@postgres:5432/notrecinema

PORT=8080
LOG_LEVEL=info
SESSION_COOKIE_NAME=notre_cinema_session
ALLOWED_ORIGINS=https://${domain}
APP_ENV=production
OTLP_ENDPOINT=jaeger:4318

TMDB_API_KEY=
OMDB_API_KEY=
KINOPOISK_API_KEY=
TRAKT_CLIENT_ID=
GITHUB_CLIENT_ID=
GITHUB_CLIENT_SECRET=

# Картинки: SeaweedFS внутри сети compose, наружу через nginx (media.*).
STORAGE_ENDPOINT=http://seaweedfs:8333
STORAGE_REGION=us-east-1
STORAGE_ACCESS_KEY=${storage_access_key}
STORAGE_SECRET_KEY=${storage_secret_key}
STORAGE_BUCKET=notrecinema-images
STORAGE_PUBLIC_URL=https://media.${domain}/notrecinema-images

# Те же VAPID-ключи, что у фронтенда (npx web-push generate-vapid-keys).
VAPID_PUBLIC_KEY=
VAPID_PRIVATE_KEY=
VAPID_SUBJECT=mailto:admin@${domain}

NUDGE_CHECK_INTERVAL=24h
NUDGE_DAYS_THRESHOLD=14
DIGEST_ENABLED=true
DIGEST_WEEKDAY=monday
DIGEST_HOUR_UTC=6
DIGEST_CHECK_INTERVAL=30m
EVENT_REMINDERS_ENABLED=true
EVENT_REMINDER_LEAD=1h
EVENT_REMINDER_INTERVAL=1m

# Почта: без RESEND_API_KEY письма отключены.
RESEND_API_KEY=
MAIL_FROM=notrecinema <noreply@${domain}>
MAIL_REPLY_TO=
APP_URL=https://${domain}
UNSUBSCRIBE_SECRET=${unsubscribe_secret}
ENV

echo "Создан $(pwd)/.env (права 600). Впишите ключи внешних сервисов:"
echo "  RESEND_API_KEY, VAPID_*, GITHUB_CLIENT_*, TMDB/OMDB/KINOPOISK"
