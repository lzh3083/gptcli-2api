#!/usr/bin/env bash
# Upgrade helper: old file/JSON backend data → PostgreSQL hybrid backend.
# Safe wrapper around migrate_json_to_pg.py
set -euo pipefail
cd "$(dirname "$0")/.."

DATA_DIR="${DATA_DIR:-./data}"
DATABASE_URL="${DATABASE_URL:-${GROK2API_DATABASE_URL:-}}"
COMPOSE="${COMPOSE:-1}"
DRY_RUN=0
MERGE_POOL=1

usage() {
  cat <<'EOF'
Usage: scripts/upgrade_from_file_backend.sh [options]

Options:
  --data-dir PATH       Path to old data/ (default: ./data)
  --database-url URL    PostgreSQL URL (or set DATABASE_URL)
  --no-compose          Do not run docker compose up for redis/postgres
  --dry-run             Pass --dry-run to migrator
  --no-merge-pool       Do not pass --merge-pool
  -h, --help            Show help

Environment:
  DATABASE_URL / GROK2API_DATABASE_URL
  DATA_DIR
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --database-url) DATABASE_URL="$2"; shift 2 ;;
    --no-compose) COMPOSE=0; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --no-merge-pool) MERGE_POOL=0; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage; exit 2 ;;
  esac
done

if [[ -z "$DATABASE_URL" ]]; then
  # Compose default for in-network app; host-side migrate needs 127.0.0.1
  if [[ -f .env ]]; then
    # shellcheck disable=SC1091
    set -a; source .env; set +a || true
    DATABASE_URL="${DATABASE_URL:-${GROK2API_DATABASE_URL:-}}"
  fi
fi
if [[ -z "$DATABASE_URL" ]]; then
  DATABASE_URL="postgresql://grok2api:grok2api@127.0.0.1:5432/grok2api"
  echo "WARN: DATABASE_URL unset; using demo default: $DATABASE_URL"
fi

echo "== upgrade from file backend =="
echo "data_dir=$DATA_DIR"
echo "database=${DATABASE_URL#*@}"

if [[ ! -d "$DATA_DIR" ]]; then
  echo "ERROR: data dir not found: $DATA_DIR" >&2
  exit 1
fi

if [[ "$COMPOSE" == "1" ]] && command -v docker >/dev/null 2>&1 && [[ -f docker-compose.yml ]]; then
  echo "== ensure redis + postgres =="
  docker compose up -d redis postgres
  echo "waiting for postgres..."
  for i in $(seq 1 40); do
    if docker compose exec -T postgres pg_isready -U grok2api -d grok2api >/dev/null 2>&1; then
      echo "postgres ready"
      break
    fi
    sleep 1
  done
fi

export DATABASE_URL
export GROK2API_DATABASE_URL="$DATABASE_URL"

ARGS=(--data-dir "$DATA_DIR" --database-url "$DATABASE_URL")
if [[ "$MERGE_POOL" == "1" ]]; then
  ARGS+=(--merge-pool)
fi
if [[ "$DRY_RUN" == "1" ]]; then
  ARGS+=(--dry-run)
fi

echo "== run migrator =="
if command -v python3 >/dev/null 2>&1; then
  python3 scripts/migrate_json_to_pg.py "${ARGS[@]}"
else
  python scripts/migrate_json_to_pg.py "${ARGS[@]}"
fi

echo
echo "Next steps:"
echo "  1) Ensure .env has GROK2API_STORE_BACKEND=hybrid and REDIS_URL / DATABASE_URL"
echo "  2) docker compose up -d"
echo "  3) curl -fsS http://127.0.0.1:3000/health"
echo "  4) Re-login admin UI (sessions are not migrated)"
echo "See docs/UPGRADE.md for details."
