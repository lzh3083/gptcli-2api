#!/usr/bin/env bash
# Main container entrypoint:
# 1) optionally start in-process Turnstile Solver on 127.0.0.1:5073  (Python captcha)
# 2) when runtime=go, start Python registration/SSO sidecar on 127.0.0.1:18080
# 3) start grokcli-2api using the selected main runtime (Go preferred; Python fallback)
set -euo pipefail
cd /app

runtime="$(echo "${GROK2API_RUNTIME:-go}" | tr '[:upper:]' '[:lower:]')"
case "${runtime}" in
  go|"")
    if [[ ! -x /app/bin/grok2api && -x ./bin/grok2api ]]; then
      APP_CMD=("./bin/grok2api")
    else
      APP_CMD=("/app/bin/grok2api")
    fi
    if [[ ! -x "${APP_CMD[0]}" ]]; then
      echo "[entrypoint] ERROR: Go binary ${APP_CMD[0]} not found/executable" >&2
      exit 2
    fi
    runtime=go
    ;;
  python)
    echo "[entrypoint] ERROR: GROK2API_RUNTIME=python is removed; only Go main + Python sidecar remain" >&2
    echo "[entrypoint] set GROK2API_RUNTIME=go (default) and keep registration/captcha sidecars" >&2
    exit 2
    ;;
  *)
    echo "[entrypoint] invalid GROK2API_RUNTIME=${GROK2API_RUNTIME}; expected go" >&2
    exit 2
    ;;
esac
if [[ "$#" -gt 0 ]]; then
  # Ignore legacy `python app.py` CMD leftovers from older images/docs.
  if [[ "$#" -eq 2 && "$1" == "python" && "$2" == "app.py" ]]; then
    :
  elif [[ "$#" -eq 1 && "$1" == "/app/bin/grok2api" ]]; then
    :
  else
    APP_CMD=("$@")
  fi
fi

provider="$(echo "${GROK2API_CAPTCHA_PROVIDER:-${CAPTCHA_PROVIDER:-local}}" | tr '[:upper:]' '[:lower:]')"
enable_solver="${GROK2API_INLINE_SOLVER:-1}"
solver_port="${TURNSTILE_PORT:-5073}"
# Keep captcha browser pool size aligned with registration concurrency.
reg_concurrency="${GROK2API_REG_CONCURRENCY:-3}"
solver_thread="${TURNSTILE_THREAD:-${reg_concurrency}}"
solver_browser="${TURNSTILE_BROWSER_TYPE:-camoufox}"
solver_host="${TURNSTILE_HOST:-127.0.0.1}"
solver_pid=""
reg_pid=""
xvfb_pid=""

start_xvfb() {
  if command -v Xvfb >/dev/null 2>&1; then
    export DISPLAY="${DISPLAY:-:99}"
    if ! pgrep -x Xvfb >/dev/null 2>&1; then
      # Clear lock/socket left behind by an unclean shutdown; otherwise Xvfb
      # exits immediately with "Server is already active for display 99" and
      # every non-headless Chromium start fails.
      rm -f "/tmp/.X${DISPLAY#:}-lock" "/tmp/.X11-unix/X${DISPLAY#:}" 2>/dev/null || true
      echo "[entrypoint] starting Xvfb on display ${DISPLAY}"
      # setsid detaches Xvfb from this shell's process group. Without it the EXIT
      # trap below fired the moment this script exec'd the Go runtime and killed
      # Xvfb right after starting it — leaving a stale /tmp/.X11-unix/X99 socket
      # and making every non-headless Chromium fail with "浏览器连接失败"
      # (which in turn forced --headless=new and got us Cloudflare-blocked).
      setsid Xvfb "${DISPLAY}" -screen 0 1920x1080x24 -nolisten tcp >/tmp/xvfb.log 2>&1 < /dev/null &
      sleep 1
    fi
  fi
}

# Python package roots used by registration/SSO/captcha sidecars.
export PYTHONPATH="${PYTHONPATH:-/app:/app/grok-build-auth}"
case ":${PYTHONPATH}:" in
  *":/app:"*) ;;
  *) export PYTHONPATH="/app:${PYTHONPATH}" ;;
esac
case ":${PYTHONPATH}:" in
  *":/app/grok-build-auth:"*) ;;
  *) export PYTHONPATH="${PYTHONPATH}:/app/grok-build-auth" ;;
esac

start_inline_solver() {
  if [[ ! -f /app/turnstile-solver/api_solver.py ]]; then
    echo "[entrypoint] turnstile-solver missing; skip inline solver"
    return 0
  fi
  mkdir -p /app/turnstile-solver/logs /app/turnstile-solver/keys
  # Lazy browsers (default): pool warms on first captcha, reclaims after idle.
  # TURNSTILE_LAZY=0 restores eager warm-up. TURNSTILE_IDLE_SEC=0 disables reclaim.
  export TURNSTILE_LAZY="${TURNSTILE_LAZY:-1}"
  export TURNSTILE_IDLE_SEC="${TURNSTILE_IDLE_SEC:-180}"
  echo "[entrypoint] starting Python turnstile-solver on ${solver_host}:${solver_port} (thread=${solver_thread}, browser=${solver_browser}, lazy=${TURNSTILE_LAZY}, idle=${TURNSTILE_IDLE_SEC}s)"
  # --proxy makes api_solver.py actually read proxies.txt. Without it the solver
  # egresses from the container IP while the registration browser egresses from
  # the configured proxy, and Cloudflare rejects the solved token because the
  # challenge and its redemption come from different addresses.
  (
    cd /app/turnstile-solver
    exec python api_solver.py \
      --browser_type "${solver_browser}" \
      --thread "${solver_thread}" \
      --host "${solver_host}" \
      --port "${solver_port}" \
      --proxy \
      --debug
  ) > /app/turnstile-solver/logs/turnstile_solver.log 2>&1 &
  solver_pid=$!
  echo "${solver_pid}" > /app/turnstile-solver/logs/turnstile_solver.pid
  echo "[entrypoint] turnstile-solver pid=${solver_pid}"

  # Wait until solver HTTP is ready (best-effort, but fail loud if still down)
  for i in $(seq 1 90); do
    if curl -fsS -m 1 "http://127.0.0.1:${solver_port}/health" >/dev/null 2>&1 \
      || curl -fsS -m 1 "http://127.0.0.1:${solver_port}/" >/dev/null 2>&1; then
      echo "[entrypoint] turnstile-solver ready"
      return 0
    fi
    if ! kill -0 "${solver_pid}" 2>/dev/null; then
      echo "[entrypoint] WARN: turnstile-solver exited early; see turnstile-solver/logs/turnstile_solver.log" >&2
      return 0
    fi
    sleep 1
  done
  echo "[entrypoint] WARN: turnstile-solver not ready after 90s; registration will wait/block until it is" >&2
}

start_registration_sidecar() {
  local reg_host reg_port wait_sec
  reg_host="${GROK2API_REGISTRATION_HOST:-127.0.0.1}"
  reg_port="${GROK2API_REGISTRATION_PORT:-18080}"
  wait_sec="${GROK2API_REGISTRATION_READY_WAIT_SEC:-30}"
  export GROK2API_REGISTRATION_SERVICE_URL="${GROK2API_REGISTRATION_SERVICE_URL:-http://${reg_host}:${reg_port}}"
  # Force loopback local solver for in-container captcha path.
  export GROK2API_LOCAL_SOLVER_URL="${GROK2API_LOCAL_SOLVER_URL:-http://127.0.0.1:${solver_port}}"
  export LOCAL_SOLVER_URL="${LOCAL_SOLVER_URL:-http://127.0.0.1:${solver_port}}"

  if [[ ! -f /app/scripts/registration_service.py ]]; then
    echo "[entrypoint] WARN: scripts/registration_service.py missing; SSO/registration admin facade unavailable" >&2
    return 0
  fi
  mkdir -p /app/turnstile-solver/logs
  echo "[entrypoint] starting Python registration/SSO sidecar on ${reg_host}:${reg_port}"
  (
    cd /app
    export PYTHONDONTWRITEBYTECODE=1
    exec python3 -B scripts/registration_service.py
  ) > /app/turnstile-solver/logs/registration_sidecar.log 2>&1 &
  reg_pid=$!
  echo "${reg_pid}" > /app/turnstile-solver/logs/registration_sidecar.pid
  echo "[entrypoint] registration sidecar pid=${reg_pid} url=${GROK2API_REGISTRATION_SERVICE_URL}"

  for i in $(seq 1 "${wait_sec}"); do
    if curl -fsS -m 1 "${GROK2API_REGISTRATION_SERVICE_URL}/health" >/dev/null 2>&1; then
      echo "[entrypoint] registration/SSO sidecar ready"
      return 0
    fi
    if ! kill -0 "${reg_pid}" 2>/dev/null; then
      echo "[entrypoint] WARN: registration sidecar exited early; see turnstile-solver/logs/registration_sidecar.log" >&2
      return 0
    fi
    sleep 1
  done
  echo "[entrypoint] WARN: registration sidecar not ready after ${wait_sec}s; Go facades will 503 until it is" >&2
}

stop_pid() {
  local name="$1"
  local pid="$2"
  if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
    echo "[entrypoint] stopping ${name} pid=${pid}"
    kill "${pid}" 2>/dev/null || true
    sleep 1
    kill -9 "${pid}" 2>/dev/null || true
  fi
}

# Apply SQL schema before the Go app starts. The app only verifies
# schema_migrations (no auto-migrate inside the binary); Docker/fresh Postgres
# therefore needs this step or /ready fails with:
#   verify migrations: ERROR: relation "schema_migrations" does not exist
run_migrations() {
  local auto_migrate migrate_bin migrate_dir wait_sec i
  auto_migrate="$(echo "${GROK2API_AUTO_MIGRATE:-1}" | tr '[:upper:]' '[:lower:]')"
  case "${auto_migrate}" in
    0|false|no|off)
      echo "[entrypoint] GROK2API_AUTO_MIGRATE=${auto_migrate}; skip schema migrate"
      return 0
      ;;
  esac

  migrate_bin="${GROK2API_MIGRATE_BIN:-}"
  if [[ -z "${migrate_bin}" ]]; then
    if [[ -x /app/bin/grok2api-migrate ]]; then
      migrate_bin="/app/bin/grok2api-migrate"
    elif [[ -x ./bin/grok2api-migrate ]]; then
      migrate_bin="./bin/grok2api-migrate"
    fi
  fi
  migrate_dir="${GROK2API_MIGRATIONS_DIR:-migrations}"
  if [[ ! -x "${migrate_bin:-}" ]]; then
    echo "[entrypoint] ERROR: migrate binary not found/executable (set GROK2API_MIGRATE_BIN or ship bin/grok2api-migrate)" >&2
    return 2
  fi
  if [[ ! -d "${migrate_dir}" ]]; then
    echo "[entrypoint] ERROR: migrations directory missing: ${migrate_dir}" >&2
    return 2
  fi

  wait_sec="${GROK2API_MIGRATE_WAIT_SEC:-60}"
  echo "[entrypoint] waiting for PostgreSQL then applying migrations (dir=${migrate_dir}, timeout=${wait_sec}s)"
  # Retry while Postgres is still coming up (compose depends_on healthy is best-effort).
  # Permanent failures (checksum mismatch, bad SQL) also retry until timeout, then exit 1.
  for i in $(seq 1 "${wait_sec}"); do
    if "${migrate_bin}" -dir "${migrate_dir}" up; then
      echo "[entrypoint] schema migrations applied/verified"
      return 0
    fi
    sleep 1
  done
  echo "[entrypoint] ERROR: failed to apply schema migrations after ${wait_sec}s" >&2
  echo "[entrypoint] hint: ensure postgres is healthy and GROK2API_DATABASE_URL/DATABASE_URL is correct" >&2
  return 1
}

cleanup() {
  stop_pid "registration sidecar" "${reg_pid}"
  stop_pid "turnstile-solver" "${solver_pid}"
  # Xvfb is deliberately NOT stopped here: it is started with setsid and must
  # survive this script exec'ing the Go runtime (see start_xvfb).
}
trap cleanup EXIT INT TERM

# Start virtual display for browser automation and solver
start_xvfb

# Schema first: sidecars and the Go app both expect applied migrations.
if ! run_migrations; then
  exit 1
fi

# Force local solver URL to loopback when using inline mode. This only keeps the
# existing registration/solver sidecar path available; the Go runtime does not
# implement captcha solving or registration execution.
if [[ "${provider}" == "local" && "${enable_solver}" != "0" ]]; then
  export GROK2API_CAPTCHA_PROVIDER=local
  export CAPTCHA_PROVIDER=local
  export GROK2API_LOCAL_SOLVER_URL="http://127.0.0.1:${solver_port}"
  export LOCAL_SOLVER_URL="http://127.0.0.1:${solver_port}"
  start_inline_solver
fi

# When the main runtime is Go, keep Python only for registration/captcha/SSO.
# The registration sidecar is loopback-only and implements:
#   /internal/registration/v1/*
#   /internal/sso/v1/*
if [[ "${runtime}" == "go" && "${GROK2API_REGISTRATION_SIDECAR:-1}" != "0" ]]; then
  start_registration_sidecar
fi

echo "[entrypoint] starting app (${runtime}): ${APP_CMD[*]}"
exec "${APP_CMD[@]}"
