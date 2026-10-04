#!/usr/bin/env bash
# Start grokcli-2api (Go main process). Python remains only as sidecars:
# SSO conversion, registration machine, turnstile captcha solver.
set -euo pipefail
cd "$(dirname "$0")"

if [[ ! -f .env ]]; then
  if [[ -f .env.example ]]; then
    cp .env.example .env
    echo "Created .env from .env.example — edit secrets (admin password, mail keys) as needed."
  else
    echo "WARN: .env.example missing; continuing with process environment only." >&2
  fi
fi
if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

export GROK2API_RUNTIME="${GROK2API_RUNTIME:-go}"
export GROK2API_OPEN_BROWSER="${GROK2API_OPEN_BROWSER:-0}"
export GROK2API_HOST="${GROK2API_HOST:-0.0.0.0}"
export GROK2API_PORT="${GROK2API_PORT:-8081}"
export GROK2API_ACCOUNT_MODE="${GROK2API_ACCOUNT_MODE:-round_robin}"
export GROK2API_TOKEN_MAINTAIN="${GROK2API_TOKEN_MAINTAIN:-1}"
export GROK2API_REASONING_COMPAT="${GROK2API_REASONING_COMPAT:-off}"
export GROK2API_WORKERS="${GROK2API_WORKERS:-1}"
export GROK2API_STORE_BACKEND="${GROK2API_STORE_BACKEND:-hybrid}"
export REDIS_URL="${REDIS_URL:-${GROK2API_REDIS_URL:-redis://127.0.0.1:6379/0}}"
export DATABASE_URL="${DATABASE_URL:-${GROK2API_DATABASE_URL:-postgresql://grok2api:grok2api@127.0.0.1:5432/grok2api}}"
export GROK2API_REDIS_URL="${GROK2API_REDIS_URL:-$REDIS_URL}"
export GROK2API_DATABASE_URL="${GROK2API_DATABASE_URL:-$DATABASE_URL}"
export GROK2API_GO_PUBLIC_READ="${GROK2API_GO_PUBLIC_READ:-1}"
export GROK2API_GO_CHAT="${GROK2API_GO_CHAT:-1}"
export GROK2API_GO_MESSAGES="${GROK2API_GO_MESSAGES:-1}"
export GROK2API_GO_RESPONSES="${GROK2API_GO_RESPONSES:-1}"
export GROK2API_GO_ADMIN_READ="${GROK2API_GO_ADMIN_READ:-1}"
export GROK2API_GO_ADMIN_WRITE="${GROK2API_GO_ADMIN_WRITE:-1}"
export GROK2API_GO_MAINTAINER="${GROK2API_GO_MAINTAINER:-1}"
export GROK2API_GO_WRITES="${GROK2API_GO_WRITES:-1}"
export GROK2API_GO_OWNERSHIP_MODE="${GROK2API_GO_OWNERSHIP_MODE:-all}"
export GROK2API_REGISTRATION_SIDECAR="${GROK2API_REGISTRATION_SIDECAR:-1}"
export GROK2API_REGISTRATION_SERVICE_URL="${GROK2API_REGISTRATION_SERVICE_URL:-http://127.0.0.1:18080}"
export GROK2API_CAPTCHA_PROVIDER="${GROK2API_CAPTCHA_PROVIDER:-local}"
export GROK2API_INLINE_SOLVER="${GROK2API_INLINE_SOLVER:-1}"
export GROK2API_REG_CONCURRENCY="${GROK2API_REG_CONCURRENCY:-3}"
export TURNSTILE_THREAD="${TURNSTILE_THREAD:-${GROK2API_REG_CONCURRENCY:-3}}"
export TURNSTILE_BROWSER_TYPE="${TURNSTILE_BROWSER_TYPE:-camoufox}"
export TURNSTILE_PORT="${TURNSTILE_PORT:-5073}"
export PYTHONPATH="$(pwd):$(pwd)/grok-build-auth:$(pwd)/grok-build-auth/openai_client${PYTHONPATH:+:$PYTHONPATH}"

BIN=""
if [[ -x ./bin/grok2api ]]; then
  BIN=./bin/grok2api
elif [[ -x /app/bin/grok2api ]]; then
  BIN=/app/bin/grok2api
elif command -v go >/dev/null 2>&1; then
  echo "Building Go binary..."
  mkdir -p bin
  go build -o bin/grok2api ./cmd/grok2api
  BIN=./bin/grok2api
else
  echo "ERROR: bin/grok2api missing and Go toolchain not found." >&2
  exit 1
fi

PORT="$GROK2API_PORT"
echo "Starting grokcli-2api (Go main + Python sidecars)..."
echo "  Admin:     http://127.0.0.1:${PORT}/admin"
echo "  Health:    http://127.0.0.1:${PORT}/health"
echo "  OpenAI:    http://127.0.0.1:${PORT}/v1"
echo "  Runtime:   go"
echo "  Redis:     ${REDIS_URL}"
echo "  Database:  ${DATABASE_URL}"
echo "  Binary:    ${BIN}"
echo ""

# Prefer full entrypoint when available (starts captcha + registration sidecars).
if [[ -x ./entrypoint.sh ]]; then
  exec ./entrypoint.sh "${BIN}"
fi
exec "${BIN}"
