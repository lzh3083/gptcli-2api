@echo off
REM Go main process. Prefer WSL/Docker for full sidecar stack.
cd /d %~dp0
if not exist bin\grok2api.exe (
  echo Building bin\grok2api.exe ...
  go build -o bin\grok2api.exe .\cmd\grok2api
)
set GROK2API_RUNTIME=go
set GROK2API_GO_PUBLIC_READ=1
set GROK2API_GO_CHAT=1
set GROK2API_GO_MESSAGES=1
set GROK2API_GO_RESPONSES=1
set GROK2API_GO_ADMIN_READ=1
set GROK2API_GO_ADMIN_WRITE=1
set GROK2API_GO_MAINTAINER=1
set GROK2API_GO_WRITES=1
bin\grok2api.exe
