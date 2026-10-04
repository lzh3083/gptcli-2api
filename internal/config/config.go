package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHost           = "0.0.0.0"
	defaultPort           = 8081
	defaultModel          = "gpt-4o"
	defaultUpstream       = "https://api.openai.com/v1"
	defaultRedisURL       = "redis://127.0.0.1:6379/0"
	defaultDatabaseURL    = "postgresql://grok2api:grok2api@127.0.0.1:5432/grok2api"
	defaultRedisPrefix    = "g2a"
	defaultSSEKeepalive   = 4 * time.Second
	defaultRequestTimeout = 900 * time.Second
	defaultStaticDir      = "static"
)

// Config is the startup-only subset needed by the staged Go runtime. New
// settings are added only with Python parity tests and a manifest entry.
type Config struct {
	Host                            string
	Port                            int
	DefaultModel                    string
	UpstreamBase                    string
	RedisURL                        string
	DatabaseURL                     string
	RedisPrefix                     string
	StoreBackend                    string
	LegacyAPIKey                    string
	LegacyAdminPassword             string
	RequireAPIKey                   string
	RequireSharedStores             bool
	RequireMigrations               bool
	Runtime                         string
	GoPublicRead                    bool
	GoChat                          bool
	GoMessages                      bool
	GoResponses                     bool
	GoAdminRead                     bool
	GoAdminWrite                    bool
	GoMaintainer                    bool
	GoWrites                        bool
	GoOwnershipMode                 string
	RegistrationMode                string
	RegistrationServiceURL          string
	RegistrationToken               string
	StaticDir                       string
	SSEKeepalive                    time.Duration
	RequestTimeout                  time.Duration
	OutboundMaxTools                int
	OutboundMaxToolsOpenAI          int
	OutboundMaxToolsResponsesNative int
	OutboundToolGap                 time.Duration
	OutboundToolGapNative           time.Duration
	StickyFirstOnly                 bool
	FirstByteProbeWorkers           int // parallel failover first-byte probes (after sticky miss)
	CodexForceReasoningLow          bool
	Workers                         int
	MaintainerLeader                string
	MaintainerLeaderTTL             time.Duration
	MaintainerLeaderRenew           time.Duration
}

func Load() (Config, error) {
	port, err := envInt("GROK2API_PORT", defaultPort, 1, 65535)
	if err != nil {
		return Config{}, err
	}
	keepalive, err := envSeconds("GROK2API_SSE_KEEPALIVE", defaultSSEKeepalive, 0, 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	timeout, err := envSeconds("GROK2API_TIMEOUT", defaultRequestTimeout, time.Second, 24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	requireShared, err := envBool("GROK2API_REQUIRE_SHARED_STORES", true)
	if err != nil {
		return Config{}, err
	}
	requireMigrations, err := envBool("GROK2API_REQUIRE_MIGRATIONS", true)
	if err != nil {
		return Config{}, err
	}
	goPublicRead, err := envBool("GROK2API_GO_PUBLIC_READ", true)
	if err != nil {
		return Config{}, err
	}
	goChat, err := envBool("GROK2API_GO_CHAT", true)
	if err != nil {
		return Config{}, err
	}
	goMessages, err := envBool("GROK2API_GO_MESSAGES", true)
	if err != nil {
		return Config{}, err
	}
	goResponses, err := envBool("GROK2API_GO_RESPONSES", true)
	if err != nil {
		return Config{}, err
	}
	goAdminRead, err := envBool("GROK2API_GO_ADMIN_READ", true)
	if err != nil {
		return Config{}, err
	}
	goAdminWrite, err := envBool("GROK2API_GO_ADMIN_WRITE", true)
	if err != nil {
		return Config{}, err
	}
	goMaintainer, err := envBool("GROK2API_GO_MAINTAINER", true)
	if err != nil {
		return Config{}, err
	}
	goWrites, err := envBool("GROK2API_GO_WRITES", true)
	if err != nil {
		return Config{}, err
	}
	outboundMaxTools, err := envInt("GROK2API_OUTBOUND_MAX_TOOLS", 1, 0, 64)
	if err != nil {
		return Config{}, err
	}
	outboundMaxToolsOpenAI, err := envInt("GROK2API_OUTBOUND_MAX_TOOLS_OPENAI", 0, 0, 64)
	if err != nil {
		return Config{}, err
	}
	outboundMaxToolsResponsesNative, err := envInt("GROK2API_OUTBOUND_MAX_TOOLS_RESPONSES_NATIVE", 0, 0, 64)
	if err != nil {
		return Config{}, err
	}
	outboundToolGap, err := envSeconds("GROK2API_OUTBOUND_TOOL_GAP_SEC", 80*time.Millisecond, 0, 2*time.Second)
	if err != nil {
		return Config{}, err
	}
	outboundToolGapNative, err := envSeconds("GROK2API_OUTBOUND_TOOL_GAP_SEC_NATIVE", 0, 0, 2*time.Second)
	if err != nil {
		return Config{}, err
	}
	stickyFirstOnly, err := envBool("GROK2API_STICKY_FIRST_ONLY", true)
	if err != nil {
		return Config{}, err
	}
	firstByteProbeWorkers, err := envInt("GROK2API_FIRST_BYTE_PROBE_WORKERS", 3, 1, 8)
	if err != nil {
		return Config{}, err
	}
	codexForceReasoningLow, err := envBool("GROK2API_CODEX_FORCE_REASONING_LOW", true)
	if err != nil {
		return Config{}, err
	}

	storeBackend, err := envEnum("GROK2API_STORE_BACKEND", "hybrid", "hybrid", "file")
	if err != nil {
		return Config{}, err
	}
	// Python public API runtime was removed; only "go" is accepted (entrypoint also rejects "python").
	runtime, err := envEnum("GROK2API_RUNTIME", "go", "go")
	if err != nil {
		return Config{}, err
	}
	ownershipMode, err := envEnum("GROK2API_GO_OWNERSHIP_MODE", "all", "disabled", "canary", "all")
	if err != nil {
		return Config{}, err
	}
	registrationMode, err := envEnum("GROK2API_REGISTRATION_MODE", "external", "external")
	if err != nil {
		return Config{}, err
	}
	workers, err := envInt("GROK2API_WORKERS", 1, 1, 256)
	if err != nil {
		return Config{}, err
	}
	leaderTTL, err := envSeconds("GROK2API_MAINTAINER_LEADER_TTL", 30*time.Second, 5*time.Second, 10*time.Minute)
	if err != nil {
		return Config{}, err
	}
	leaderRenew, err := envSeconds("GROK2API_MAINTAINER_LEADER_RENEW", 10*time.Second, 2*time.Second, 5*time.Minute)
	if err != nil {
		return Config{}, err
	}

	return Config{
		Host:                            envString("GROK2API_HOST", defaultHost),
		Port:                            port,
		DefaultModel:                    envString("GROK2API_DEFAULT_MODEL", defaultModel),
		UpstreamBase:                    strings.TrimRight(envString("GROK_CLI_CHAT_PROXY_BASE_URL", defaultUpstream), "/"),
		RedisURL:                        envAlias([]string{"GROK2API_REDIS_URL", "REDIS_URL"}, defaultRedisURL),
		DatabaseURL:                     envAlias([]string{"GROK2API_DATABASE_URL", "DATABASE_URL"}, defaultDatabaseURL),
		RedisPrefix:                     envString("GROK2API_REDIS_PREFIX", defaultRedisPrefix),
		StoreBackend:                    storeBackend,
		LegacyAPIKey:                    envString("GROK2API_API_KEY", ""),
		LegacyAdminPassword:             envString("GROK2API_ADMIN_PASSWORD", ""),
		RequireAPIKey:                   strings.ToLower(strings.TrimSpace(envString("GROK2API_REQUIRE_API_KEY", "auto"))),
		RequireSharedStores:             requireShared,
		RequireMigrations:               requireMigrations,
		Runtime:                         runtime,
		GoPublicRead:                    goPublicRead,
		GoChat:                          goChat,
		GoMessages:                      goMessages,
		GoResponses:                     goResponses,
		GoAdminRead:                     goAdminRead,
		GoAdminWrite:                    goAdminWrite,
		GoMaintainer:                    goMaintainer,
		GoWrites:                        goWrites,
		GoOwnershipMode:                 ownershipMode,
		RegistrationMode:                registrationMode,
		RegistrationServiceURL:          strings.TrimRight(envString("GROK2API_REGISTRATION_SERVICE_URL", ""), "/"),
		RegistrationToken:               envString("GROK2API_REGISTRATION_TOKEN", ""),
		StaticDir:                       envString("GROK2API_STATIC_DIR", defaultStaticDir),
		SSEKeepalive:                    keepalive,
		RequestTimeout:                  timeout,
		OutboundMaxTools:                outboundMaxTools,
		OutboundMaxToolsOpenAI:          outboundMaxToolsOpenAI,
		OutboundMaxToolsResponsesNative: outboundMaxToolsResponsesNative,
		OutboundToolGap:                 outboundToolGap,
		OutboundToolGapNative:           outboundToolGapNative,
		StickyFirstOnly:                 stickyFirstOnly,
		FirstByteProbeWorkers:           firstByteProbeWorkers,
		CodexForceReasoningLow:          codexForceReasoningLow,
		Workers:                         workers,
		MaintainerLeader:                strings.ToLower(strings.TrimSpace(envString("GROK2API_MAINTAINER_LEADER", "auto"))),
		MaintainerLeaderTTL:             leaderTTL,
		MaintainerLeaderRenew:           leaderRenew,
	}, nil
}

func (c Config) Address() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envAlias(names []string, fallback string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return fallback
}

func envInt(name string, fallback, minimum, maximum int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

func envBool(name string, fallback bool) (bool, error) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		return fallback, nil
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be a boolean", name)
	}
}

func envEnum(name, fallback string, values ...string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		raw = fallback
	}
	for _, value := range values {
		if raw == value {
			return raw, nil
		}
	}
	return "", fmt.Errorf("%s must be one of %s", name, strings.Join(values, ", "))
}

func envSeconds(name string, fallback, minimum, maximum time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be seconds: %w", name, err)
	}
	value := time.Duration(seconds * float64(time.Second))
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be between %s and %s", name, minimum, maximum)
	}
	return value, nil
}

// ApplyStoreSettings overlays durable app_settings values onto the process config.
// Only keys that affect live request routing / streaming are applied; maintainer
// enable flags are read dynamically via PublicSettings and are not required here.
func (c *Config) ApplyStoreSettings(settings map[string]any) {
	if c == nil || settings == nil {
		return
	}
	if v, ok := settings["default_model"].(string); ok {
		if s := strings.TrimSpace(v); s != "" {
			c.DefaultModel = s
		}
	}
	if v, ok := asSettingsFloat(settings["sse_keepalive"]); ok && v >= 2 && v <= 120 {
		c.SSEKeepalive = time.Duration(v * float64(time.Second))
	}
	if v, ok := asSettingsInt(settings["outbound_max_tools"]); ok && v >= 0 && v <= 64 {
		c.OutboundMaxTools = v
	}
	if v, ok := asSettingsInt(settings["outbound_max_tools_openai"]); ok && v >= 0 && v <= 64 {
		c.OutboundMaxToolsOpenAI = v
	}
	if v, ok := asSettingsFloat(settings["outbound_tool_gap_sec"]); ok && v >= 0 && v <= 2 {
		c.OutboundToolGap = time.Duration(v * float64(time.Second))
	}
}

func asSettingsFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func asSettingsInt(value any) (int, bool) {
	f, ok := asSettingsFloat(value)
	if !ok {
		return 0, false
	}
	return int(f), true
}
