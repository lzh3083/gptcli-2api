#!/usr/bin/env bash
# Local development for the Go main process.
# Usage:
#   ./dev.sh
#   GROK2API_PORT=40081 ./dev.sh
#
# Requires Redis + PostgreSQL (same as production hybrid mode).
# Example with docker only for stores:
#   docker compose up -d postgres redis
#   ./dev.sh
#
# Python is only needed for registration/SSO/captcha sidecars.
set -euo pipefail
cd "$(dirname "$0")"

if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env
  set +a
fi

export GROK2API_RUNTIME=go
export GROK2API_WORKERS="${GROK2API_WORKERS:-1}"
export GROK2API_OPEN_BROWSER="${GROK2API_OPEN_BROWSER:-0}"
export GROK2API_HOST="${GROK2API_HOST:-0.0.0.0}"
export GROK2API_PORT="${GROK2API_PORT:-8081}"
export GROK2API_STORE_BACKEND="${GROK2API_STORE_BACKEND:-hybrid}"
export REDIS_URL="${REDIS_URL:-${GROK2API_REDIS_URL:-redis://127.0.0.1:6379/0}}"
export DATABASE_URL="${DATABASE_URL:-${GROK2API_DATABASE_URL:-postgresql://grok2api:grok2api@127.0.0.1:5432/grok2api}}"
export GROK2API_REDIS_URL="${GROK2API_REDIS_URL:-$REDIS_URL}"
export GROK2API_DATABASE_URL="${GROK2API_DATABASE_URL:-$DATABASE_URL}"
export GROK2API_GO_PUBLIC_READ=1
export GROK2API_GO_CHAT=1
export GROK2API_GO_MESSAGES=1
export GROK2API_GO_RESPONSES=1
export GROK2API_GO_ADMIN_READ=1
export GROK2API_GO_ADMIN_WRITE=1
export GROK2API_GO_MAINTAINER=1
export GROK2API_GO_WRITES=1
export GROK2API_GO_OWNERSHIP_MODE="${GROK2API_GO_OWNERSHIP_MODE:-all}"
export GROK2API_REGISTRATION_SIDECAR="${GROK2API_REGISTRATION_SIDECAR:-1}"
export GROK2API_REGISTRATION_SERVICE_URL="${GROK2API_REGISTRATION_SERVICE_URL:-http://127.0.0.1:18080}"
export PYTHONPATH="$(pwd):$(pwd)/grok-build-auth:$(pwd)/grok-build-auth/openai_client${PYTHONPATH:+:$PYTHONPATH}"

if [[ -f scripts/build_admin_assets.py && "${GROK2API_BUILD_ASSETS_ON_START:-0}" == "1" ]]; then
  python3 scripts/build_admin_assets.py || echo "WARN: admin asset build failed (continuing)" >&2
fi

echo "Building Go binary..."
mkdir -p bin
go build -o bin/grok2api ./cmd/grok2api

echo "Dev starting (Go; rebuild manually after code changes)..."
echo "  Admin:  http://127.0.0.1:${GROK2API_PORT}/admin"
echo "  Health: http://127.0.0.1:${GROK2API_PORT}/health"
echo ""

if [[ -x ./entrypoint.sh ]]; then
  exec ./entrypoint.sh ./bin/grok2api
fi
exec ./bin/grok2api
