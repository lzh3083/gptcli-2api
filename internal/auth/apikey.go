package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hm2899/grokcli-2api/internal/config"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
)

var ErrInvalidAPIKey = errors.New("invalid or missing API key")

type APIKeyRecord struct {
	ID           string
	Name         string
	Prefix       string
	KeyHash      string
	Enabled      bool
	RequestCount int64
	LastUsedAt   *time.Time
}

type APIKeyVerifier struct {
	cfg   config.Config
	store *postgres.Connector

	mu            sync.Mutex
	keyCache      map[string]apiKeyCacheEntry
	requiredCache *requiredCacheEntry
}

type apiKeyCacheEntry struct {
	rec     *APIKeyRecord
	expires time.Time
}

type requiredCacheEntry struct {
	required bool
	expires  time.Time
}

const (
	apiKeyCacheTTL       = 30 * time.Second
	authRequiredCacheTTL = 5 * time.Second
)

func NewAPIKeyVerifier(cfg config.Config, store *postgres.Connector) *APIKeyVerifier {
	return &APIKeyVerifier{
		cfg:      cfg,
		store:    store,
		keyCache: map[string]apiKeyCacheEntry{},
	}
}

func (v *APIKeyVerifier) Require(ctx context.Context, r *http.Request) (*APIKeyRecord, error) {
	token := tokenFromRequest(r)
	required, err := v.AuthRequired(ctx)
	if err != nil {
		return nil, err
	}
	if !required {
		if token == "" {
			return nil, nil
		}
		return v.Verify(ctx, token)
	}
	if token == "" {
		return nil, ErrInvalidAPIKey
	}
	rec, err := v.Verify(ctx, token)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, ErrInvalidAPIKey
	}
	return rec, nil
}

func (v *APIKeyVerifier) AuthRequired(ctx context.Context) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v.cfg.RequireAPIKey)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	if strings.TrimSpace(v.cfg.LegacyAPIKey) != "" {
		return true, nil
	}
	if v.store == nil {
		return false, nil
	}
	now := time.Now()
	v.mu.Lock()
	if v.requiredCache != nil && now.Before(v.requiredCache.expires) {
		required := v.requiredCache.required
		v.mu.Unlock()
		return required, nil
	}
	v.mu.Unlock()

	required, err := v.store.HasEnabledAPIKeys(ctx)
	if err != nil {
		return false, err
	}
	v.mu.Lock()
	v.requiredCache = &requiredCacheEntry{required: required, expires: now.Add(authRequiredCacheTTL)}
	v.mu.Unlock()
	return required, nil
}

func (v *APIKeyVerifier) Verify(ctx context.Context, token string) (*APIKeyRecord, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, nil
	}
	if legacy := strings.TrimSpace(v.cfg.LegacyAPIKey); legacy != "" && constantTimeEqual(token, legacy) {
		h := hashKey(token)
		return &APIKeyRecord{ID: "env", Name: "env:GROK2API_API_KEY", Prefix: prefix(token), KeyHash: h, Enabled: true}, nil
	}
	if v.store == nil {
		return nil, nil
	}
	h := hashKey(token)
	now := time.Now()
	v.mu.Lock()
	if entry, ok := v.keyCache[h]; ok && now.Before(entry.expires) {
		rec := cloneAPIKeyRecord(entry.rec)
		v.mu.Unlock()
		if rec != nil {
			// Async usage touch — never block TTFT.
			go func(id string) {
				bg, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
				defer cancel()
				_ = v.store.TouchAPIKeyUsage(bg, id)
			}(rec.ID)
		}
		return rec, nil
	}
	v.mu.Unlock()

	row, err := v.store.FindAPIKeyByHash(ctx, h)
	if err != nil || row == nil || !row.Enabled {
		return nil, err
	}
	rec := &APIKeyRecord{
		ID:           row.ID,
		Name:         row.Name,
		Prefix:       row.Prefix,
		KeyHash:      row.KeyHash,
		Enabled:      row.Enabled,
		RequestCount: row.RequestCount,
		LastUsedAt:   row.LastUsedAt,
	}
	v.mu.Lock()
	v.keyCache[h] = apiKeyCacheEntry{rec: cloneAPIKeyRecord(rec), expires: now.Add(apiKeyCacheTTL)}
	// Opportunistic prune.
	if len(v.keyCache) > 2048 {
		for k, e := range v.keyCache {
			if now.After(e.expires) {
				delete(v.keyCache, k)
			}
		}
	}
	v.mu.Unlock()

	// Async usage touch so auth never waits on PG UPDATE.
	go func(id string) {
		bg, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		_ = v.store.TouchAPIKeyUsage(bg, id)
	}(rec.ID)
	return rec, nil
}

func cloneAPIKeyRecord(in *APIKeyRecord) *APIKeyRecord {
	if in == nil {
		return nil
	}
	out := *in
	if in.LastUsedAt != nil {
		t := *in.LastUsedAt
		out.LastUsedAt = &t
	}
	return &out
}

func tokenFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	if authorization := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		return strings.TrimSpace(authorization[7:])
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func prefix(raw string) string {
	if len(raw) >= 12 {
		return raw[:12]
	}
	return raw
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// InvalidateKey removes a specific key hash from the in-memory cache so that
// admin disable/delete operations take effect immediately instead of waiting
// for the 30-second TTL to expire.
func (v *APIKeyVerifier) InvalidateKey(keyHash string) {
	if v == nil {
		return
	}
	v.mu.Lock()
	delete(v.keyCache, keyHash)
	v.mu.Unlock()
}

// InvalidateAllKeys clears the entire key cache.
func (v *APIKeyVerifier) InvalidateAllKeys() {
	if v == nil {
		return
	}
	v.mu.Lock()
	v.keyCache = map[string]apiKeyCacheEntry{}
	v.requiredCache = nil
	v.mu.Unlock()
}
