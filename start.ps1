# Go main process. Prefer WSL/Docker for registration/captcha sidecars.
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot
if (-not (Test-Path "bin\grok2api.exe")) {
  Write-Host "Building bin\grok2api.exe ..."
  go build -o bin\grok2api.exe .\cmd\grok2api
}
$env:GROK2API_RUNTIME = "go"
$env:GROK2API_GO_PUBLIC_READ = "1"
$env:GROK2API_GO_CHAT = "1"
$env:GROK2API_GO_MESSAGES = "1"
$env:GROK2API_GO_RESPONSES = "1"
$env:GROK2API_GO_ADMIN_READ = "1"
$env:GROK2API_GO_ADMIN_WRITE = "1"
$env:GROK2API_GO_MAINTAINER = "1"
$env:GROK2API_GO_WRITES = "1"
& ".\bin\grok2api.exe"
