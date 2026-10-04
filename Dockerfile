# grokcli-2api — single container with optional inline Turnstile Solver
FROM golang:1.24-bookworm AS go-builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN go build -o /out/grok2api ./cmd/grok2api \
    && go build -o /out/grok2api-migrate ./cmd/grok2api-migrate

FROM python:3.12-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    TZ=Asia/Shanghai \
    GROK2API_HOST=0.0.0.0 \
    GROK2API_PORT=8081 \
    GROK2API_OPEN_BROWSER=0 \
    GROK2API_STORE_BACKEND=hybrid \
    GROK2API_RUNTIME=go \
    GROK2API_GO_PUBLIC_READ=1 \
    GROK2API_GO_CHAT=1 \
    GROK2API_GO_MESSAGES=1 \
    GROK2API_GO_RESPONSES=1 \
    GROK2API_GO_ADMIN_READ=1 \
    GROK2API_GO_ADMIN_WRITE=1 \
    GROK2API_GO_MAINTAINER=1 \
    GROK2API_GO_WRITES=1 \
    GROK2API_GO_OWNERSHIP_MODE=all \
    GROK2API_WORKERS=2 \
    # App code + vendored registration protocol client
    PYTHONPATH=/app:/app/grok-build-auth:/app/grok-build-auth/openai_client \
    HOME=/root \
    DEBIAN_FRONTEND=noninteractive \
    # Inline local captcha defaults (same container, Python)
    GROK2API_CAPTCHA_PROVIDER=local \
    CAPTCHA_PROVIDER=local \
    GROK2API_LOCAL_SOLVER_URL=http://127.0.0.1:5073 \
    LOCAL_SOLVER_URL=http://127.0.0.1:5073 \
    GROK2API_INLINE_SOLVER=1 \
    TURNSTILE_HOST=127.0.0.1 \
    TURNSTILE_PORT=5073 \
    TURNSTILE_THREAD=3 \
    TURNSTILE_BROWSER_TYPE=camoufox \
    TURNSTILE_LAZY=1 \
    TURNSTILE_IDLE_SEC=180 \
    # Python registration/SSO sidecar (loopback only; used when RUNTIME=go)
    GROK2API_REGISTRATION_SIDECAR=1 \
    GROK2API_REGISTRATION_HOST=127.0.0.1 \
    GROK2API_REGISTRATION_PORT=18080 \
    GROK2API_REGISTRATION_SERVICE_URL=http://127.0.0.1:18080

WORKDIR /app

# App tools + browser runtime libs for inline Turnstile Solver (Camoufox/Firefox)
# Static docker CLI for in-container hot-update (needs docker.sock mount at runtime).
ARG DOCKER_CLI_VERSION=27.5.1
ARG TARGETARCH
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        chromium \
        curl \
        fonts-liberation \
        fonts-noto-color-emoji \
        nodejs \
        libasound2 \
        libatk-bridge2.0-0 \
        libatk1.0-0 \
        libcups2 \
        libdbus-1-3 \
        libdrm2 \
        libgbm1 \
        libgtk-3-0 \
        libnspr4 \
        libnss3 \
        libpango-1.0-0 \
        libx11-6 \
        libx11-xcb1 \
        libxcb1 \
        libxcomposite1 \
        libxdamage1 \
        libxext6 \
        libxfixes3 \
        libxkbcommon0 \
        libxrandr2 \
        libxshmfence1 \
        libxss1 \
        libxtst6 \
        tzdata \
        xvfb \
    && ln -snf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo Asia/Shanghai > /etc/timezone \
    && arch="${TARGETARCH:-$(dpkg --print-architecture)}" \
    && case "$arch" in \
         amd64|x86_64) darch=x86_64 ;; \
         arm64|aarch64) darch=aarch64 ;; \
         *) darch=x86_64 ;; \
       esac \
    && curl -fsSL "https://download.docker.com/linux/static/stable/${darch}/docker-${DOCKER_CLI_VERSION}.tgz" \
         | tar -xz -C /tmp \
    && mv /tmp/docker/docker /usr/local/bin/docker \
    && chmod +x /usr/local/bin/docker \
    && rm -rf /tmp/docker \
    && docker --version \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt /app/requirements.txt
COPY requirements-store.txt /app/requirements-store.txt
COPY turnstile-solver/requirements.txt /app/turnstile-solver-requirements.txt
RUN python -m pip install --no-cache-dir -U pip setuptools wheel \
    && python -m pip install --no-cache-dir -r /app/requirements.txt \
    && python -m pip install --no-cache-dir -r /app/requirements-store.txt \
    && python -m pip install --no-cache-dir -r /app/turnstile-solver-requirements.txt

# Prefetch browser binaries used by inline solver
RUN python -m camoufox fetch \
    && python -m patchright install chromium || true

COPY . /app
COPY --from=go-builder /out/grok2api /app/bin/grok2api
COPY --from=go-builder /out/grok2api-migrate /app/bin/grok2api-migrate
RUN chmod +x /app/entrypoint.sh /app/bin/grok2api /app/bin/grok2api-migrate \
    && mkdir -p /app/turnstile-solver/logs /app/turnstile-solver/keys \
    && test -f /app/grok-build-auth/xconsole_client/client.py \
    && test -f /app/grok2api/upstream/grok_build_adapter.py \
    && test -f /app/grok2api/admin/sso_import.py \
    && test -f /app/turnstile-solver/api_solver.py \
    && test -f /app/scripts/registration_service.py \
    && test -f /app/scripts/sso_to_auth_json.py \
    && test -f /app/scripts/g2a-hot-update-incontainer.sh \
    && chmod +x /app/scripts/g2a-hot-update-incontainer.sh \
    && test -x /app/bin/grok2api \
    && test -x /app/bin/grok2api-migrate \
    && python -c "from grok2api.upstream import grok_build_adapter; from grok2api.admin import sso_import; import scripts.registration_service as regsvc; print('build-check', grok_build_adapter.ADAPTER_BUILD, 'sso-import-ok', 'reg-sidecar-ok')"

EXPOSE 8081 5073

# data/ only for optional JSON import artifacts / models cache
VOLUME ["/app/data"]

ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["/app/bin/grok2api"]
