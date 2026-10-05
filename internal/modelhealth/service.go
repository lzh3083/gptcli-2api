package modelhealth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hm2899/grokcli-2api/internal/pool"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
	"github.com/hm2899/grokcli-2api/internal/store/redis"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

// Default probe knobs mirror Python grok2api.pool.model_health.
const (
	// Large-pool defaults: ~7k accounts / batch 120 / interval 180s ≈ ~3h full sweep.
	// Override via GROK2API_MODEL_PROBE_BATCH / WORKERS / MODEL_HEALTH_INTERVAL.
	defaultWorkers            = 12
	defaultBatch              = 120
	defaultMaxModelsPerAcct   = 2
	defaultCycleBudget        = 150 * time.Second
	defaultManualCycleBudget  = 150 * time.Second
	defaultProbeTimeout       = 12 * time.Second
	defaultManualAccountLimit = 5000
	// Multi-wave manual_all: keep each wave short (lock-friendly) but continue
	// until the live pool is covered or maxWaves is hit.
	defaultManualMaxWaves   = 80
	defaultManualWaveBudget = 90 * time.Second
	// Full-job wall clock for async probe-all (multi-wave).
	defaultManualJobTimeout = 45 * time.Minute
	defaultSweepTTLSec      = 12 * 3600
)

type Service struct {
	Store               *postgres.Connector
	Redis               *redis.Client
	Upstream            string
	Models              []string
	Interval            time.Duration
	Batch               int
	Workers             int
	MaxModelsPerAccount int
	CycleBudget         time.Duration
	ManualCycleBudget   time.Duration
	ManualMaxWaves      int
	AutoDisable         bool
	Enabled             func() bool
	IsLeader            func() bool

	mu         sync.Mutex
	started    bool
	stop       chan struct{}
	runSoon    chan struct{}
	last       map[string]any
	modelRR    int
	httpClient *http.Client

	// Async manual_all job (admin "全部模型探测").
	jobMu      sync.Mutex
	job        map[string]any
	jobCancel  context.CancelFunc
	jobRunning bool

	// Local fallback when Redis sweep is unavailable.
	localSweepMu      sync.Mutex
	localSweepGen     int64
	localSweepStart   float64
	localSweepCovered map[string]struct{}
}

func New(store *postgres.Connector, redisClient *redis.Client, upstream string, models []string) *Service {
	models = normalizeModels(models)
	if len(models) == 0 {
		models = normalizeModels(splitCSV(os.Getenv("GROK2API_PROBE_MODELS")))
	}
	if len(models) == 0 {
		models = []string{"gpt-6.1-sol", "gpt-6-astra", "gpt-6-luna"}
	}
	return &Service{
		Store:    store,
		Redis:    redisClient,
		Upstream: strings.TrimRight(upstream, "/"),
		Models:   models,
		// Default interval 3m (was 15m) so large pools finish a sweep in hours, not days.
		Interval:            envDurationSec("GROK2API_MODEL_HEALTH_INTERVAL", 3*time.Minute, 30*time.Second, 2*time.Hour),
		Batch:               envInt("GROK2API_MODEL_PROBE_BATCH", defaultBatch, 1, 1000),
		Workers:             envInt("GROK2API_MODEL_PROBE_WORKERS", defaultWorkers, 1, 64),
		MaxModelsPerAccount: envInt("GROK2API_MODEL_PROBE_MAX_MODELS_PER_ACCOUNT", defaultMaxModelsPerAcct, 1, 16),
		CycleBudget:         envDurationSec("GROK2API_MODEL_PROBE_CYCLE_BUDGET", defaultCycleBudget, 15*time.Second, 4*time.Minute),
		ManualCycleBudget:   envDurationSec("GROK2API_MODEL_PROBE_MANUAL_BUDGET", defaultManualCycleBudget, 30*time.Second, 4*time.Minute),
		AutoDisable:         true,
		ManualMaxWaves:      envInt("GROK2API_MODEL_PROBE_MANUAL_MAX_WAVES", defaultManualMaxWaves, 1, 200),
		Enabled:             func() bool { return true },
		IsLeader:            func() bool { return true },
		stop:                make(chan struct{}),
		runSoon:             make(chan struct{}, 1),
		last:                map[string]any{"ok": true, "started": false},
		httpClient:          newProbeHTTPClient(),
		job:                 map[string]any{"running": false},
		localSweepCovered:   map[string]struct{}{},
	}
}

// Configure hot-applies knobs from durable settings / admin without restart.
// Zero / negative values leave the current field unchanged.
func (s *Service) Configure(intervalSec float64, batch, workers int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if intervalSec >= 30 {
		d := time.Duration(intervalSec * float64(time.Second))
		if d > 2*time.Hour {
			d = 2 * time.Hour
		}
		s.Interval = d
	}
	if batch >= 1 {
		if batch > 1000 {
			batch = 1000
		}
		s.Batch = batch
	}
	if workers >= 1 {
		if workers > 64 {
			workers = 64
		}
		s.Workers = workers
	}
}

// Knobs returns current interval/batch/workers for admin UI.
func (s *Service) Knobs() (interval time.Duration, batch, workers int) {
	if s == nil {
		return 0, 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Interval, s.Batch, s.Workers
}

// SetModels hot-updates the probed models list.
func (s *Service) SetModels(models []string) {
	if s == nil {
		return
	}
	norm := normalizeModels(models)
	if len(norm) == 0 {
		norm = []string{"gpt-6.1-sol", "gpt-6-astra", "gpt-6-luna"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Models = norm
}

func newProbeHTTPClient() *http.Client {
	// Shared client for dense probe batches: reuse TLS/TCP across accounts.
	// Sized for MODEL_PROBE_WORKERS up to 32 without thrashing the dialer.
	return &http.Client{
		Timeout: defaultProbeTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          128,
			MaxIdleConnsPerHost:   64,
			MaxConnsPerHost:       96,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			ForceAttemptHTTP2:     true,
			DisableKeepAlives:     false,
			DialContext: (&net.Dialer{
				Timeout:   8 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
}

func (s *Service) probeHTTP() *http.Client {
	if s == nil {
		return newProbeHTTPClient()
	}
	// httpClient is set in New and only replaced under rare config reloads;
	// read without lock on the hot probe path to avoid worker contention.
	if c := s.httpClient; c != nil {
		return c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpClient == nil {
		s.httpClient = newProbeHTTPClient()
	}
	return s.httpClient
}

func (s *Service) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.loop()
}

func (s *Service) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	close(s.stop)
	s.stop = make(chan struct{})
	s.mu.Unlock()
}

func (s *Service) RequestRunSoon() {
	select {
	case s.runSoon <- struct{}{}:
	default:
	}
}

func (s *Service) Status() map[string]any {
	if s == nil {
		return map[string]any{"enabled": false, "implementation": "go", "started": false, "running": false}
	}
	s.mu.Lock()
	lastCopy := map[string]any{}
	for k, v := range s.last {
		lastCopy[k] = v
	}
	started := s.started
	interval := s.Interval
	batch := s.Batch
	workers := s.Workers
	models := append([]string{}, s.Models...)
	s.mu.Unlock()

	enabled := s.Enabled == nil || s.Enabled()
	isLeader := s.IsLeader == nil || s.IsLeader()
	running := started && enabled && isLeader

	if lastCopy != nil {
		if lastCopy["count"] == nil && lastCopy["probed"] != nil {
			lastCopy["count"] = lastCopy["probed"]
		}
		if lastCopy["available_count"] == nil && lastCopy["available"] != nil {
			lastCopy["available_count"] = lastCopy["available"]
		}
		if lastCopy["unavailable_count"] == nil && lastCopy["failed"] != nil {
			lastCopy["unavailable_count"] = lastCopy["failed"]
		}
		if lastCopy["models"] == nil {
			lastCopy["models"] = models
			lastCopy["models_configured"] = models
		}
	}

	out := map[string]any{
		"enabled":         enabled,
		"started":         started,
		"running":         running,
		"local_running":   running,
		"cluster_running": running,
		"leader_running":  running,
		"implementation":  "go",
		"interval_sec":    interval.Seconds(),
		"probe_batch":     batch,
		"batch":           batch,
		"probe_workers":   workers,
		"workers":         workers,
		"models":          models,
		"probe_models":    models,
		"is_leader":       isLeader,
		"last":            lastCopy,
		"selection":       "strict_sweep",
	}
	s.jobMu.Lock()
	jobCopy := map[string]any{}
	for k, v := range s.job {
		jobCopy[k] = v
	}
	s.jobMu.Unlock()
	out["job"] = jobCopy
	if s.Store != nil && batch > 0 && interval > 0 {
		if n, err := s.Store.CountEnabledAccounts(context.Background()); err == nil && n > 0 {
			// Match runWave adaptive batch so ETA is not wildly pessimistic.
			effBatch := batch
			if workers > 0 {
				adaptive := workers * 15
				if adaptive > effBatch {
					effBatch = adaptive
				}
				if effBatch > 400 {
					effBatch = 400
				}
			}
			if effBatch < 1 {
				effBatch = 1
			}
			out["probe_batch_effective"] = effBatch
			cycles := (int(n) + effBatch - 1) / effBatch
			out["full_pool_eta_sec"] = float64(cycles) * interval.Seconds()
			covered, gen, mode := s.sweepSnapshot(int(n))
			remaining := int(n) - covered
			if remaining < 0 {
				remaining = int(n)
			}
			out["sweep"] = map[string]any{
				"mode": mode, "live": n, "remaining": remaining, "covered": covered,
				"generation": gen,
			}
		}
	}
	return out
}

func (s *Service) loop() {
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
			s.maybeRun()
			timer.Reset(s.Interval)
		case <-s.runSoon:
			s.maybeRun()
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.Interval)
		}
	}
}

func (s *Service) maybeRun() {
	if s.Enabled != nil && !s.Enabled() {
		return
	}
	if s.IsLeader != nil && !s.IsLeader() {
		return
	}
	// Never race background waves against admin "全部模型探测" (shared lock + sweep).
	s.jobMu.Lock()
	manualBusy := s.jobRunning
	s.jobMu.Unlock()
	if manualBusy {
		slog.Debug("model health background skipped: manual_all running")
		return
	}
	// Background cycles stay inside the maintenance lock budget (~3 min).
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result := s.RunOnce(ctx, "background")
	s.mu.Lock()
	s.last = result
	s.mu.Unlock()
	s.writeTaskLog(ctx, "background", result)
}

func (s *Service) RunOnce(ctx context.Context, source string) map[string]any {
	// Background: one priority batch under cycle budget.
	// manual_all: multi-wave until pool covered or max waves / job cancel.
	if source == "manual_all" || source == "manual" || source == "admin" {
		return s.runManualAll(ctx, source)
	}
	return s.runWave(ctx, source, false, false)
}

// StartProbeAll starts (or returns) an async full-pool probe job.
// Admin UI can poll JobStatus / model-health Status().job.
func (s *Service) StartProbeAll() map[string]any {
	if s == nil {
		return map[string]any{"ok": false, "error": "service unavailable"}
	}
	s.jobMu.Lock()
	if s.jobRunning {
		job := cloneMap(s.job)
		s.jobMu.Unlock()
		job["ok"] = true
		job["already_running"] = true
		return job
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultManualJobTimeout)
	s.jobCancel = cancel
	s.jobRunning = true
	started := time.Now()
	jobID := "manual_all:" + time.Now().UTC().Format("20060102T150405")
	s.job = map[string]any{
		"ok": true, "running": true, "source": "manual_all",
		"job_id": jobID, "task_id": "probe:" + jobID,
		"started_at": started.Unix(), "wave": 0, "waves": 0,
		"probed": 0, "available": 0, "failed": 0,
		"implementation": "go",
	}
	jobSnap := cloneMap(s.job)
	s.jobMu.Unlock()

	// Fresh sweep generation for this manual job so we do not inherit a half-covered
	// background generation (which made "全部模型探测" look stuck / incomplete).
	_ = s.startSweep(context.Background())

	go func() {
		result := s.runManualAll(ctx, "manual_all")
		cancel()
		s.jobMu.Lock()
		s.jobRunning = false
		s.jobCancel = nil
		result["running"] = false
		result["finished_at"] = time.Now().Unix()
		if result["ok"] == nil {
			result["ok"] = true
		}
		s.job = result
		s.jobMu.Unlock()
		s.mu.Lock()
		s.last = result
		s.mu.Unlock()
	}()
	return jobSnap
}

// JobStatus returns the current/last async probe-all job snapshot.
func (s *Service) JobStatus() map[string]any {
	if s == nil {
		return map[string]any{"running": false}
	}
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	return cloneMap(s.job)
}

func (s *Service) runManualAll(ctx context.Context, source string) map[string]any {
	startedAt := time.Now()
	result := map[string]any{"ok": true, "source": source, "implementation": "go", "at": startedAt.Unix()}
	if s.Store == nil {
		result["ok"] = false
		result["error"] = "store unavailable"
		return result
	}

	// Hold model_health lock for the whole multi-wave job so background waves
	// cannot sneak between waves (intermittent deferred_busy → 0/0).
	skipWaveLock := false
	if s.Redis != nil && s.Redis.Enabled() {
		lockTTL := 5 * time.Minute
		lockCtx, lockCancel := context.WithTimeout(ctx, 3*time.Minute)
		ok, release, err := s.Redis.AcquireMaintenanceLock(lockCtx, "model_health", lockTTL, true)
		lockCancel()
		if err == nil && ok {
			defer release()
			skipWaveLock = true
			result["job_lock"] = true
		} else if err == nil && !ok {
			slog.Warn("model health manual_all: job lock busy; per-wave locks will be used")
			result["job_lock"] = false
			result["job_lock_busy"] = true
		} else if err != nil {
			slog.Warn("model health manual_all: job lock error", "error", err)
			result["lock_error"] = err.Error()
		}
	}

	maxWaves := s.ManualMaxWaves
	if maxWaves <= 0 {
		maxWaves = defaultManualMaxWaves
	}
	// Manual waves use a shorter per-wave budget so the maintenance lock renews between waves.
	waveBudget := s.ManualCycleBudget
	if waveBudget <= 0 {
		waveBudget = defaultManualWaveBudget
	}
	if waveBudget > 2*time.Minute {
		// Keep each lock hold reasonable; multi-wave covers the rest.
		waveBudget = 2 * time.Minute
	}
	if waveBudget < 30*time.Second {
		waveBudget = 30 * time.Second
	}

	totalAvailable, totalFailed, totalProbes, totalAccounts := 0, 0, 0, 0
	kickCooldown, kickDisabled, modelBlocks, recovered := 0, 0, 0, 0
	samples := []map[string]any{}
	var lastModels []string
	var lastWorkers int
	waves := 0
	budgetHitFinal := false
	busySkips := 0
	emptySkips := 0

	for wave := 1; wave <= maxWaves; wave++ {
		if ctx.Err() != nil {
			budgetHitFinal = true
			break
		}
		// Per-wave context with its own budget; parent cancel still stops the job.
		waveCtx, cancel := context.WithTimeout(ctx, waveBudget+30*time.Second)
		waveRes := s.runWave(waveCtx, source, true, skipWaveLock)
		cancel()
		waves = wave

		probed := intOf(waveRes["probed"])
		count := intOf(waveRes["count"])
		avail := intOf(waveRes["available_count"])
		if avail == 0 {
			avail = intOf(waveRes["available"])
		}
		failed := intOf(waveRes["unavailable_count"])
		if failed == 0 {
			failed = intOf(waveRes["failed"])
		}
		totalAccounts += probed
		totalProbes += count
		totalAvailable += avail
		totalFailed += failed
		kickCooldown += intOf(waveRes["kick_cooldown"])
		kickDisabled += intOf(waveRes["kick_disabled"])
		modelBlocks += intOf(waveRes["model_blocked_count"])
		recovered += intOf(waveRes["recovered"])
		if ms, ok := waveRes["failed_sample"].([]map[string]any); ok {
			for _, row := range ms {
				if len(samples) < 8 {
					samples = append(samples, row)
				}
			}
		}
		if m, ok := waveRes["models"].([]string); ok {
			lastModels = m
		}
		lastWorkers = intOf(waveRes["workers"])
		if waveRes["budget_hit"] == true {
			budgetHitFinal = true
		}

		// Progress snapshot for UI polling.
		s.jobMu.Lock()
		if s.jobRunning {
			s.job["wave"] = wave
			s.job["waves"] = wave
			s.job["probed"] = totalAccounts
			s.job["count"] = totalProbes
			s.job["available"] = totalAvailable
			s.job["available_count"] = totalAvailable
			s.job["failed"] = totalFailed
			s.job["unavailable_count"] = totalFailed
			s.job["running"] = true
			s.job["elapsed_ms"] = time.Since(startedAt).Milliseconds()
			if sw, ok := waveRes["sweep"].(map[string]any); ok {
				s.job["sweep"] = sw
			}
			if m, ok := waveRes["models"].([]string); ok && len(m) > 0 {
				s.job["models"] = m
			}
			if w := intOf(waveRes["workers"]); w > 0 {
				s.job["workers"] = w
			}
		}
		s.jobMu.Unlock()

		// Lock-busy waves return no useful sweep remaining. Handle deferred_busy
		// BEFORE the "empty pool" exit, otherwise intermittent 0/0 happens whenever
		// background health still holds the model_health lock for a few seconds.
		if waveRes["deferred_busy"] == true {
			result["deferred_busy"] = true
			busySkips++
			if waveRes["error"] != nil {
				result["error"] = waveRes["error"]
			}
			s.jobMu.Lock()
			if s.jobRunning {
				s.job["wave"] = wave
				s.job["waves"] = wave
				s.job["running"] = true
				s.job["deferred_busy"] = true
				s.job["busy_skips"] = busySkips
				s.job["elapsed_ms"] = time.Since(startedAt).Milliseconds()
				if sw, ok := waveRes["sweep"].(map[string]any); ok {
					s.job["sweep"] = sw
				}
			}
			s.jobMu.Unlock()
			if wave < maxWaves && ctx.Err() == nil {
				// Background waves hold the lock up to ~cycle budget; wait longer than
				// 2s so we actually get a turn instead of spinning 80×2s fails.
				wait := 5 * time.Second
				if busySkips > 3 {
					wait = 15 * time.Second
				}
				if busySkips > 8 {
					wait = 30 * time.Second
				}
				select {
				case <-ctx.Done():
					budgetHitFinal = true
				case <-time.After(wait):
				}
				if ctx.Err() == nil {
					continue
				}
			}
			break
		}

		// Stop when nothing left to probe this generation.
		remaining := intOfMap(waveRes, "sweep", "remaining")
		// Busy/empty paths may omit remaining; recompute from probeable set.
		if waveRes["sweep"] == nil {
			live := 0
			if s.Store != nil {
				if n, err := s.Store.CountProbeableAccounts(ctx); err == nil {
					live = int(n)
				}
			}
			covered, _, _ := s.sweepSnapshot(live)
			remaining = live - covered
			if remaining < 0 {
				remaining = 0
			}
		}
		if remaining == 0 && probed == 0 {
			// Truly empty / fully covered — not a lock miss.
			break
		}
		if remaining == 0 {
			budgetHitFinal = false
			break
		}
		// probed==0 with remaining>0: soft-continue (SQL exclude should avoid this).
		if probed == 0 {
			emptySkips++
			if remaining > 0 && wave < maxWaves {
				select {
				case <-ctx.Done():
					budgetHitFinal = true
				case <-time.After(500 * time.Millisecond):
				}
				if ctx.Err() == nil {
					continue
				}
			}
			break
		}
	}

	if len(lastModels) == 0 {
		lastModels = s.modelsForSource(source)
	}
	result["probed"] = totalAccounts
	result["count"] = totalProbes
	result["available"] = totalAvailable
	result["available_count"] = totalAvailable
	result["failed"] = totalFailed
	result["unavailable_count"] = totalFailed
	result["auto_action_count"] = kickCooldown + kickDisabled + modelBlocks
	result["kick_cooldown"] = kickCooldown
	result["kick_disabled"] = kickDisabled
	result["model_blocked_count"] = modelBlocks
	result["recovered"] = recovered
	result["failed_sample"] = samples
	result["models"] = lastModels
	result["models_configured"] = append([]string{}, s.Models...)
	if len(lastModels) > 0 {
		result["model"] = lastModels[0]
	}
	result["workers"] = lastWorkers
	result["waves"] = waves
	result["budget_hit"] = budgetHitFinal
	result["busy_skips"] = busySkips
	result["empty_skips"] = emptySkips
	result["elapsed_ms"] = time.Since(startedAt).Milliseconds()
	if totalAccounts == 0 {
		if result["error"] == nil || result["error"] == "" {
			if result["deferred_busy"] == true {
				result["error"] = "model_health lock busy — no wave completed"
				result["ok"] = false
			} else {
				result["error"] = "no probeable accounts (all cooling, expired, disabled, or missing token)"
			}
		}
	}
	// Surface empty runs so admin UI does not show a silent "0/0 可用" as success.
	if totalAccounts == 0 && totalProbes == 0 {
		result["ok"] = false
		if result["error"] == nil {
			if result["deferred_busy"] == true {
				result["error"] = "no accounts probed: model_health lock busy"
			} else {
				result["error"] = "no probeable accounts (all cooling / disabled / expired / missing token?)"
			}
		}
	}
	// Attach current sweep snapshot (probeable set, not all enabled).
	live := 0
	if s.Store != nil {
		if n, err := s.Store.CountProbeableAccounts(ctx); err == nil {
			live = int(n)
		} else if n, err := s.Store.CountEnabledAccounts(ctx); err == nil {
			live = int(n)
		}
	}
	covered, gen, mode := s.sweepSnapshot(live)
	remaining := live - covered
	if remaining < 0 {
		remaining = 0
	}
	result["sweep"] = map[string]any{
		"mode": mode, "live": live, "covered": covered, "remaining": remaining, "generation": gen,
	}
	if remaining > 0 {
		result["deferred"] = remaining
	}
	slog.Info("model health manual_all complete",
		"waves", waves, "probed", totalAccounts, "probes", totalProbes,
		"available", totalAvailable, "failed", totalFailed,
		"covered", covered, "remaining", remaining, "elapsed_ms", result["elapsed_ms"],
	)
	// Merge multi-wave progress into one task_logs row via stable job_id.
	s.jobMu.Lock()
	if jid, ok := s.job["job_id"].(string); ok && jid != "" {
		result["job_id"] = jid
		result["task_id"] = "probe:" + jid
	}
	s.jobMu.Unlock()
	s.writeTaskLog(ctx, source, result)
	return result
}

// runWave performs one locked probe wave (background batch or one manual wave).
func (s *Service) runWave(ctx context.Context, source string, manualWave bool, skipLock bool) map[string]any {
	startedAt := time.Now()
	result := map[string]any{"ok": true, "source": source, "implementation": "go", "at": startedAt.Unix()}
	if s.Store == nil {
		result["ok"] = false
		result["error"] = "store unavailable"
		return result
	}
	// Background must yield to in-flight manual_all (shared sweep + lock).
	if !manualWave {
		s.jobMu.Lock()
		busy := s.jobRunning
		s.jobMu.Unlock()
		if busy {
			result["deferred_busy"] = true
			result["error"] = "manual_all running — background wave deferred"
			return result
		}
	}
	if !skipLock && s.Redis != nil && s.Redis.Enabled() {
		// Per-owner lock (model_health). Token maintainer uses a different key.
		// Manual_all waits longer for the lock so a concurrent background wave
		// (cycle budget ~2m) does not turn the whole job into intermittent 0/0.
		lockTTL := 180 * time.Second
		wait := lockTTL
		if manualWave {
			// Blocking wait up to one full background cycle + headroom.
			wait = 3 * time.Minute
			if wait < lockTTL {
				wait = lockTTL
			}
		}
		// Temporarily extend ctx wait for lock only when parent allows.
		lockCtx := ctx
		var lockCancel context.CancelFunc
		if manualWave {
			lockCtx, lockCancel = context.WithTimeout(ctx, wait)
		}
		ok, release, err := s.Redis.AcquireMaintenanceLock(lockCtx, "model_health", lockTTL, true)
		if lockCancel != nil {
			lockCancel()
		}
		if err == nil && ok {
			defer release()
		} else if err == nil && !ok {
			result["deferred_busy"] = true
			result["error"] = "model_health lock busy — concurrent probe wave still holding the slot"
			// Attach live sweep so callers do not treat remaining as 0/empty pool.
			liveN := 0
			if n, err2 := s.Store.CountProbeableAccounts(ctx); err2 == nil {
				liveN = int(n)
			}
			covered, gen, mode := s.sweepSnapshot(liveN)
			rem := liveN - covered
			if rem < 0 {
				rem = 0
			}
			result["sweep"] = map[string]any{
				"mode": mode, "live": liveN, "covered": covered, "remaining": rem, "generation": gen,
			}
			result["probed"] = 0
			result["count"] = 0
			return result
		} else if err != nil {
			// Redis blip: continue without lock rather than aborting with 0/0.
			slog.Warn("model health lock acquire failed; continuing without lock", "error", err, "manual", manualWave)
			result["lock_error"] = err.Error()
		}
	}

	batch := s.Batch
	if batch <= 0 {
		batch = defaultBatch
	}
	workersHint := s.Workers
	if workersHint <= 0 {
		workersHint = defaultWorkers
	}
	// Adaptive background batch: aim for ~workers*12–20 accounts/wave so a 7k
	// pool is not stuck at batch=20 for days. Cap keeps cycle budget honest.
	if !manualWave {
		adaptive := workersHint * 15
		if adaptive < batch {
			adaptive = batch
		}
		if adaptive > 400 {
			adaptive = 400
		}
		if adaptive > batch {
			batch = adaptive
		}
	}
	// Manual waves still use larger per-wave selection (up to 5000) so one wave
	// can chew through as many accounts as the budget allows.
	limit := batch
	if manualWave {
		limit = defaultManualAccountLimit
		if n, err := s.Store.CountProbeableAccounts(ctx); err == nil && n > 0 {
			if int(n) < limit {
				limit = int(n)
			}
			if limit < batch {
				limit = batch
			}
		} else if n, err := s.Store.CountEnabledAccounts(ctx); err == nil && n > 0 {
			if int(n) < limit {
				limit = int(n)
			}
			if limit < batch {
				limit = batch
			}
		}
	}

	// Over-fetch then filter by sweep covered set so we fill the batch with uncovered.
	// Prefer CountProbeableAccounts so remaining matches ListAccountAuthsForProbe
	// eligibility (sticky cool with expired until is still probeable; quota_disabled is not).
	liveN := 0
	if n, err := s.Store.CountProbeableAccounts(ctx); err == nil {
		liveN = int(n)
	} else if n, err := s.Store.CountEnabledAccounts(ctx); err == nil {
		liveN = int(n)
	}
	// Already-covered IDs (this generation) — pass to SQL so multi-wave does not
	// re-fetch the same priority top-N every wave (was probed=0 → early stop).
	stPre := s.loadSweep(ctx)
	excludeIDs := make([]string, 0, len(stPre.Covered))
	for id := range stPre.Covered {
		excludeIDs = append(excludeIDs, id)
	}
	fetchLimit := limit
	if !manualWave {
		// Background: fetch more candidates so covered filtering still fills Batch.
		fetchLimit = batch * 4
		if fetchLimit < 200 {
			fetchLimit = 200
		}
		if fetchLimit > 4000 {
			fetchLimit = 4000
		}
	} else {
		if fetchLimit < limit {
			fetchLimit = limit
		}
		if fetchLimit > 5000 {
			fetchLimit = 5000
		}
	}
	auths, err := s.Store.ListAccountAuthsForProbe(ctx, fetchLimit, excludeIDs...)
	if err != nil {
		// Fallback without exclude if ANY() fails for any reason.
		auths, err = s.Store.ListAccountAuthsForProbe(ctx, fetchLimit)
	}
	if err != nil {
		auths, err = s.Store.ListAccountAuths(ctx, fetchLimit, true)
	}
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
		return result
	}

	// Strict non-repeat sweep for background; manual_all also marks covered so
	// multi-wave does not re-probe the same accounts within the job.
	sweepInfo, uncovered := s.filterUncovered(ctx, auths, liveN, source)
	if len(uncovered) > limit {
		uncovered = uncovered[:limit]
	}
	auths = uncovered

	cycleModels := s.modelsForSource(source)
	if len(cycleModels) == 0 {
		cycleModels = []string{"gpt-6.1-sol"}
	}

	workers := s.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	if manualWave {
		boosted := workers * 2
		if boosted > 16 {
			boosted = 16
		}
		if boosted < workers {
			boosted = workers
		}
		workers = boosted
	}
	if workers > len(auths) && len(auths) > 0 {
		workers = len(auths)
	}

	budget := s.CycleBudget
	if budget <= 0 {
		budget = defaultCycleBudget
	}
	if manualWave {
		if s.ManualCycleBudget > 0 {
			budget = s.ManualCycleBudget
		}
		if budget > 2*time.Minute {
			budget = 2 * time.Minute
		}
		if budget < 30*time.Second {
			budget = 30 * time.Second
		}
	}
	if dl, ok := ctx.Deadline(); ok {
		remain := time.Until(dl)
		if remain > 0 && remain < budget {
			budget = remain
		}
	}
	budgetCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	probes := s.probeAccountsConcurrent(budgetCtx, auths, cycleModels, source, workers)

	available, failed := 0, 0
	kickCooldown, kickDisabled, modelBlocks, recovered := 0, 0, 0, 0
	samples := []map[string]any{}
	budgetHit := budgetCtx.Err() != nil
	coverIDs := make([]string, 0, len(auths))
	holdIDs := make([]string, 0)
	seenAcct := map[string]bool{}
	for _, probe := range probes {
		aid, _ := probe["account_id"].(string)
		if probe["available"] == true {
			available++
			if probe["recovered"] == true {
				recovered++
			}
			if aid != "" && !seenAcct[aid] {
				coverIDs = append(coverIDs, aid)
				seenAcct[aid] = true
			}
			continue
		}
		if probe["budget_cut"] == true {
			// Do not cover — leave for next wave.
			continue
		}
		failed++
		if probe["auto_disabled"] == true {
			kickDisabled++
		}
		if probe["kicked_cooldown"] == true {
			kickCooldown++
		}
		if probe["model_blocked"] == true {
			modelBlocks++
		}
		if len(samples) < 5 {
			samples = append(samples, probe)
		}
		// Recoverable free-usage / rate-limit stays UNcovered so the same
		// generation can re-check after cooldown (Python parity).
		if aid != "" && !seenAcct[aid] {
			errText, _ := probe["error"].(string)
			if isFreeUsageExhausted(errText) || probe["model_blocked"] == true || probe["kicked_cooldown"] == true {
				holdIDs = append(holdIDs, aid)
			} else {
				coverIDs = append(coverIDs, aid)
			}
			seenAcct[aid] = true
		}
	}
	// Mark covered after probes complete.
	if len(coverIDs) > 0 {
		coveredTotal := s.markCovered(ctx, coverIDs)
		sweepInfo["sweep_covered"] = coveredTotal
		if liveN > 0 {
			sweepInfo["sweep_remaining"] = maxInt(0, liveN-coveredTotal)
		}
	}
	if len(holdIDs) > 0 {
		sweepInfo["held_recoverable"] = len(holdIDs)
	}

	autoActions := kickCooldown + kickDisabled + modelBlocks
	result["probed"] = len(auths)
	result["count"] = len(probes)
	result["available"] = available
	result["available_count"] = available
	result["failed"] = failed
	result["unavailable_count"] = failed
	result["auto_action_count"] = autoActions
	result["kick_cooldown"] = kickCooldown
	result["kick_disabled"] = kickDisabled
	result["model_blocked_count"] = modelBlocks
	result["recovered"] = recovered
	result["failed_sample"] = samples
	result["model"] = cycleModels[0]
	result["models"] = append([]string{}, cycleModels...)
	result["models_configured"] = append([]string{}, s.Models...)
	result["models_per_account"] = len(cycleModels)
	result["workers"] = workers
	result["budget_sec"] = budget.Seconds()
	result["budget_hit"] = budgetHit
	result["elapsed_ms"] = time.Since(startedAt).Milliseconds()
	result["sweep"] = map[string]any{
		"mode":             sweepInfo["mode"],
		"live":             liveN,
		"covered":          sweepInfo["sweep_covered"],
		"remaining":        sweepInfo["sweep_remaining"],
		"generation":       sweepInfo["sweep_generation"],
		"held_recoverable": sweepInfo["held_recoverable"],
	}
	if rem, ok := sweepInfo["sweep_remaining"].(int); ok && rem > 0 {
		result["deferred"] = rem
	}
	slog.Info("model health wave",
		"probed", len(auths), "probes", len(probes),
		"available", available, "failed", failed,
		"models", cycleModels, "workers", workers, "budget_hit", budgetHit,
		"source", source, "elapsed_ms", time.Since(startedAt).Milliseconds(),
	)
	return result
}

// probeAccountsConcurrent fans out across accounts (models per account stay sequential).
// last_probe snapshots are flushed in one bulk upsert after the wave to cut DB round-trips.
func (s *Service) probeAccountsConcurrent(ctx context.Context, auths []postgres.AccountAuth, models []string, source string, workers int) []map[string]any {
	if len(auths) == 0 {
		return nil
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > len(auths) {
		workers = len(auths)
	}

	type job struct {
		auth postgres.AccountAuth
	}
	jobs := make(chan job, workers*2)
	var (
		mu      sync.Mutex
		out     = make([]map[string]any, 0, len(auths)*len(models))
		wg      sync.WaitGroup
		skipped atomic.Int64
	)

	workerFn := func() {
		defer wg.Done()
		for j := range jobs {
			if ctx.Err() != nil {
				skipped.Add(1)
				continue
			}
			// Models for one account stay sequential so one bad model cannot
			// multiply concurrent load for the same token (Python parity).
			autoDisable := s.AutoDisable
			for _, model := range models {
				if ctx.Err() != nil {
					break
				}
				// Defer last_probe write; kick/disable still apply immediately.
				probe := s.probeAccount(ctx, j.auth, model, source, autoDisable, true)
				mu.Lock()
				out = append(out, probe)
				mu.Unlock()
			}
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go workerFn()
	}

	for _, auth := range auths {
		if ctx.Err() != nil {
			skipped.Add(1)
			continue
		}
		select {
		case <-ctx.Done():
			skipped.Add(1)
		case jobs <- job{auth: auth}:
		}
	}
	close(jobs)
	wg.Wait()
	_ = skipped.Load()

	// Bulk flush last_probe (best-effort; individual SaveLastProbe already skipped).
	if s.Store != nil && len(out) > 0 {
		// Prefer newest probe per account when multi-model (last write wins in batch order).
		// Keep all rows — SaveLastProbesBatch upserts by account_id; later rows overwrite earlier.
		// To make "last model" stick, reverse-unique by account_id.
		byAcct := make(map[string]map[string]any, len(auths))
		order := make([]string, 0, len(auths))
		for _, p := range out {
			if p == nil || p["budget_cut"] == true {
				continue
			}
			aid, _ := p["account_id"].(string)
			if aid == "" {
				continue
			}
			if _, ok := byAcct[aid]; !ok {
				order = append(order, aid)
			}
			byAcct[aid] = p
		}
		batch := make([]map[string]any, 0, len(byAcct))
		for _, aid := range order {
			batch = append(batch, byAcct[aid])
		}
		if len(batch) > 0 {
			if _, err := s.Store.SaveLastProbesBatch(ctx, batch); err != nil {
				slog.Warn("model health batch last_probe failed; falling back", "error", err, "n", len(batch))
				for _, p := range batch {
					aid, _ := p["account_id"].(string)
					_ = s.Store.SaveLastProbe(ctx, aid, p)
				}
			}
		}
		// Ensure free-usage / 429 fails always enter cooldown even if Kick was skipped.
		if n, err := s.Store.RepairFreeUsageModelBlocks(ctx); err != nil {
			slog.Warn("model health cooldown repair failed", "error", err)
		} else if n > 0 {
			slog.Info("model health cooldown repair applied", "accounts", n)
		}
	}
	return out
}

func (s *Service) ProbeAccount(ctx context.Context, auth postgres.AccountAuth, model, source string) map[string]any {
	return s.probeAccount(ctx, auth, model, source, s.AutoDisable, false)
}

func (s *Service) probeAccount(ctx context.Context, auth postgres.AccountAuth, model, source string, autoDisable bool, deferSave bool) map[string]any {
	if model == "" && len(s.Models) > 0 {
		model = s.Models[0]
	}
	started := time.Now()
	base := map[string]any{
		"ok": false, "available": false,
		"account_id": auth.ID, "email": auth.Email, "model": model,
		"probed_at": started.Unix(), "source": source,
	}
	// Reuse shared transport; per-call Timeout is already on the client.
	client := &grok.Client{BaseURL: s.Upstream, HTTP: s.probeHTTP()}
	body := map[string]any{
		"model": model, "stream": true, "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	}
	resp, err := client.Open(ctx, grok.Account{ID: auth.ID, Token: auth.Token}, model, body)
	if err != nil {
		status := 0
		errText := err.Error()
		var ue *grok.UpstreamError
		if errors.As(err, &ue) {
			status = ue.Status
			errText = ue.Body
			if len(errText) > 400 {
				errText = errText[:400]
			}
		}
		// Context cancel/timeout is not an account fault — surface cleanly.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			base["status_code"] = status
			base["error"] = "probe budget exceeded"
			base["latency_ms"] = time.Since(started).Milliseconds()
			base["budget_cut"] = true
			return base
		}
		base["status_code"] = status
		base["error"] = errText
		base["latency_ms"] = time.Since(started).Milliseconds()
		// register/import probes: never cool or disable — keep new accounts 轮询中.
		srcLower := strings.ToLower(strings.TrimSpace(source))
		skipMutate := srcLower == "register" || srcLower == "import" || srcLower == "registration" || srcLower == "sso_import"
		if autoDisable && s.Store != nil && !skipMutate {

			switch {
			case status == 401 || status == 403:
				if _, e := s.Store.SetAccountEnabled(ctx, auth.ID, false); e == nil {
					base["auto_disabled"] = true
				}
			case isFreeUsageExhausted(errText) || status == 429:
				// Classify: free-usage -> account cool only; bare 429 -> short cool.
				// Free-usage must NOT write blocked_models (cool only; keep other models usable).
				decision := pool.ClassifyUpstreamFailure(status, errText, model)
				until := time.Now().Add(10 * time.Minute)
				if decision.Until != nil {
					until = *decision.Until
				} else if decision.Class == pool.ClassFreeUsage {
					until = time.Now().Add(2 * time.Hour)
				}
				// Cap cool window so probe storms cannot pin an account for days.
				maxUntil := time.Now().Add(6 * time.Hour)
				if until.After(maxUntil) {
					until = maxUntil
				}
				// Only soft-block model when classifier explicitly asks AND this is NOT free-usage.
				if decision.BlockModel && decision.Class != pool.ClassFreeUsage {
					blockModel := model
					if decision.Model != "" {
						blockModel = decision.Model
					}
					if blockModel != "" {
						if e := s.Store.BlockPoolModel(ctx, auth.ID, blockModel, &until); e == nil {
							base["model_blocked"] = true
							base["blocked_model"] = blockModel
						}
					}
				}
				sec := until.Sub(time.Now()).Seconds()
				if sec < 60 {
					sec = 60
				}
				if sec > 6*3600 {
					sec = 6 * 3600
				}
				// Prefer classifier code in reason so KickFromPool clears free-usage model blocks.
				kickReason := errText
				if decision.Code != "" && !strings.Contains(strings.ToLower(errText), strings.ToLower(decision.Code)) {
					kickReason = decision.Code + ": " + errText
				}
				if _, e2 := s.Store.KickFromPool(ctx, auth.ID, kickReason, &sec); e2 == nil {
					base["kicked_cooldown"] = true
					base["cooldown_code"] = decision.Code
					base["failure_class"] = string(decision.Class)
				}
				// Free-usage: ensure no leftover model soft-block from older probes.
				if decision.Class == pool.ClassFreeUsage {
					_ = s.Store.UnblockPoolModel(ctx, auth.ID, "")
				}
			case status >= 500:
				sec := 300.0
				if _, e := s.Store.KickFromPool(ctx, auth.ID, errText, &sec); e == nil {
					base["kicked_cooldown"] = true
				}
			}
		}
		if s.Store != nil && !deferSave {
			_ = s.Store.SaveLastProbe(ctx, auth.ID, base)
		}
		return base
	}
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
	_ = resp.Body.Close()
	if n == 0 && resp.StatusCode == 200 {
		base["status_code"] = 200
		base["error"] = "empty model output"
		base["latency_ms"] = time.Since(started).Milliseconds()
		if s.Store != nil && !deferSave {
			_ = s.Store.SaveLastProbe(ctx, auth.ID, base)
		}
		return base
	}
	base["ok"] = true
	base["available"] = true
	base["status_code"] = resp.StatusCode
	base["latency_ms"] = time.Since(started).Milliseconds()
	if s.Store != nil {
		// Clearing cooldown is still immediate so recovered accounts re-enter rotation
		// even when last_probe is deferred to the batch flush.
		if _, err := s.Store.ClearAccountCooldown(ctx, auth.ID); err == nil {
			base["recovered"] = true
		}
		// Successful probe for this model clears its soft/hard block (Python parity).
		if model != "" {
			if err := s.Store.UnblockPoolModel(ctx, auth.ID, model); err == nil {
				base["unblocked_model"] = model
			}
		}
		if !deferSave {
			_ = s.Store.SaveLastProbe(ctx, auth.ID, base)
		}
	}
	return base
}

func (s *Service) ProbeIDs(ctx context.Context, ids []string, model string, autoDisable bool, source string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	if len(ids) == 0 {
		return out
	}

	// Resolve auths first (cheap DB lookups) then fan out probes.
	type resolved struct {
		id   string
		auth *postgres.AccountAuth
		err  error
	}
	resolvedList := make([]resolved, 0, len(ids))
	for _, id := range ids {
		auth, err := s.Store.GetAccountAuth(ctx, id)
		resolvedList = append(resolvedList, resolved{id: id, auth: auth, err: err})
	}

	workers := s.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	if workers > len(resolvedList) {
		workers = len(resolvedList)
	}

	results := make([]map[string]any, len(resolvedList))
	jobs := make(chan int, workers*2)
	var wg sync.WaitGroup

	probeModel := firstNonEmpty(model, firstModel(s.Models))
	workerFn := func() {
		defer wg.Done()
		for i := range jobs {
			r := resolvedList[i]
			if r.err != nil {
				results[i] = map[string]any{"ok": false, "account_id": r.id, "error": r.err.Error()}
				continue
			}
			// deferSave=true: return probe results first; last_probe flushed async below.
			// Kick/Clear/Block still apply immediately during probe.
			probe := s.probeAccount(ctx, *r.auth, probeModel, source, autoDisable, true)
			ok := probe["available"] == true
			results[i] = map[string]any{"ok": ok, "account_id": r.auth.ID, "email": r.auth.Email, "result": probe}
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go workerFn()
	}
	for i := range resolvedList {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	// Flush deferred last_probe after probes complete so admin HTTP can return
	// live results without waiting on per-row PG writes first.
	if s.Store != nil {
		batch := make([]map[string]any, 0, len(results))
		for _, row := range results {
			res, _ := row["result"].(map[string]any)
			if res == nil || res["budget_cut"] == true {
				continue
			}
			batch = append(batch, res)
		}
		if len(batch) > 0 {
			go func(batch []map[string]any) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := s.Store.SaveLastProbesBatch(ctx, batch); err != nil {
					for _, p := range batch {
						aid, _ := p["account_id"].(string)
						_ = s.Store.SaveLastProbe(ctx, aid, p)
					}
				}
			}(batch)
		}
	}

	kickCooldown, kickDisabled, modelBlocks, available := 0, 0, 0, 0
	for _, row := range results {
		out = append(out, row)
		if row["ok"] == true {
			available++
		}
		if res, ok := row["result"].(map[string]any); ok {
			if res["auto_disabled"] == true {
				kickDisabled++
			}
			if res["kicked_cooldown"] == true {
				kickCooldown++
			}
			if res["model_blocked"] == true {
				modelBlocks++
			}
		}
	}
	s.mu.Lock()
	s.last = map[string]any{
		"ok": true, "source": source, "implementation": "go", "at": time.Now().Unix(),
		"probed": len(ids), "count": len(ids),
		"available": available, "available_count": available,
		"failed": len(ids) - available, "unavailable_count": len(ids) - available,
		"auto_action_count": kickCooldown + kickDisabled + modelBlocks,
		"kick_cooldown":     kickCooldown, "kick_disabled": kickDisabled, "model_blocked_count": modelBlocks,
		"model": probeModel, "models": []string{probeModel},
		"workers": workers,
	}
	s.mu.Unlock()
	return out
}

// modelsForSource picks which models to probe this cycle.
// Background rotates ONE model (accounts×models explosion avoidance).
// Manual/admin may probe a small cap of models per account.
func (s *Service) modelsForSource(source string) []string {
	s.mu.Lock()
	models := append([]string{}, s.Models...)
	s.mu.Unlock()
	models = normalizeModels(models)
	if len(models) == 0 {
		return []string{"gpt-6.1-sol"}
	}
	if source == "background" {
		if len(models) == 1 {
			return models
		}
		s.mu.Lock()
		idx := s.modelRR % len(models)
		s.modelRR = (idx + 1) % len(models)
		picked := models[idx]
		s.mu.Unlock()
		return []string{picked}
	}
	capN := s.MaxModelsPerAccount
	if capN <= 0 {
		capN = defaultMaxModelsPerAcct
	}
	if capN > len(models) {
		capN = len(models)
	}
	return models[:capN]
}

// filterUncovered drops accounts already covered in the current sweep generation.
// Starts a new generation when none exists or the previous generation finished.
func (s *Service) filterUncovered(ctx context.Context, auths []postgres.AccountAuth, liveN int, source string) (map[string]any, []postgres.AccountAuth) {
	info := map[string]any{
		"mode":             "strict_sweep",
		"sweep_generation": int64(0),
		"sweep_covered":    0,
		"sweep_live":       liveN,
		"sweep_remaining":  liveN,
		"sweep_reset":      false,
	}
	if len(auths) == 0 {
		return info, nil
	}
	// Manual sequential: still use covered set so multi-wave does not re-hit.
	st := s.loadSweep(ctx)
	if st.Generation <= 0 {
		st = s.startSweep(ctx)
		info["sweep_reset"] = true
	}
	// If previous generation already covered entire live pool, start fresh
	// (background continuous sweep; manual_all also wants a full pass).
	if liveN > 0 && st.CoveredN >= liveN {
		st = s.startSweep(ctx)
		info["sweep_reset"] = true
	}
	info["sweep_generation"] = st.Generation
	info["sweep_covered"] = st.CoveredN
	info["sweep_remaining"] = maxInt(0, liveN-st.CoveredN)

	out := make([]postgres.AccountAuth, 0, len(auths))
	for _, a := range auths {
		if _, ok := st.Covered[a.ID]; ok {
			continue
		}
		out = append(out, a)
	}
	// If everything in the candidate list is covered but remaining > 0, the
	// priority query only returned already-covered rows — try unmarked by
	// returning empty so caller can stop or next wave re-fetches after mark.
	info["candidates"] = len(auths)
	info["selected"] = len(out)
	return info, out
}

func (s *Service) loadSweep(ctx context.Context) redis.SweepState {
	if s.Redis != nil && s.Redis.Enabled() {
		if st, err := s.Redis.LoadModelHealthSweep(ctx); err == nil && st.Generation > 0 {
			return st
		}
		if st, err := s.Redis.LoadModelHealthSweep(ctx); err == nil {
			return st
		}
	}
	s.localSweepMu.Lock()
	defer s.localSweepMu.Unlock()
	cov := make(map[string]struct{}, len(s.localSweepCovered))
	for k := range s.localSweepCovered {
		cov[k] = struct{}{}
	}
	return redis.SweepState{
		Generation: s.localSweepGen,
		StartedAt:  s.localSweepStart,
		Covered:    cov,
		CoveredN:   len(cov),
	}
}

func (s *Service) startSweep(ctx context.Context) redis.SweepState {
	if s.Redis != nil && s.Redis.Enabled() {
		if st, err := s.Redis.StartModelHealthSweep(ctx, s.sweepTTLSec()); err == nil {
			// Mirror locally for Status() without Redis round-trip.
			s.localSweepMu.Lock()
			s.localSweepGen = st.Generation
			s.localSweepStart = st.StartedAt
			s.localSweepCovered = map[string]struct{}{}
			s.localSweepMu.Unlock()
			return st
		}
	}
	now := time.Now()
	s.localSweepMu.Lock()
	defer s.localSweepMu.Unlock()
	s.localSweepGen = now.Unix()
	s.localSweepStart = float64(now.Unix())
	s.localSweepCovered = map[string]struct{}{}
	return redis.SweepState{
		Generation: s.localSweepGen,
		StartedAt:  s.localSweepStart,
		Covered:    map[string]struct{}{},
		CoveredN:   0,
	}
}

func (s *Service) markCovered(ctx context.Context, ids []string) int {
	if len(ids) == 0 {
		st := s.loadSweep(ctx)
		return st.CoveredN
	}
	if s.Redis != nil && s.Redis.Enabled() {
		if n, err := s.Redis.MarkModelHealthCovered(ctx, ids, s.sweepTTLSec()); err == nil {
			// Mirror local
			s.localSweepMu.Lock()
			if s.localSweepCovered == nil {
				s.localSweepCovered = map[string]struct{}{}
			}
			for _, id := range ids {
				s.localSweepCovered[id] = struct{}{}
			}
			s.localSweepMu.Unlock()
			return int(n)
		}
	}
	s.localSweepMu.Lock()
	defer s.localSweepMu.Unlock()
	if s.localSweepCovered == nil {
		s.localSweepCovered = map[string]struct{}{}
	}
	for _, id := range ids {
		s.localSweepCovered[id] = struct{}{}
	}
	return len(s.localSweepCovered)
}

func (s *Service) sweepSnapshot(liveN int) (covered int, gen int64, mode string) {
	mode = "strict_sweep"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st := s.loadSweep(ctx)
	covered = st.CoveredN
	gen = st.Generation
	if liveN > 0 && covered > liveN {
		covered = liveN
	}
	return covered, gen, mode
}

func (s *Service) sweepTTLSec() int {
	// Keep at least 6h, or ~3× full estimated coverage window (Python parity).
	interval := s.Interval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	sec := int(interval.Seconds() * 36)
	if sec < defaultSweepTTLSec {
		sec = defaultSweepTTLSec
	}
	if sec < 6*3600 {
		sec = 6 * 3600
	}
	return sec
}

func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	default:
		return 0
	}
}

func intOfMap(m map[string]any, keys ...string) int {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = mm[k]
	}
	return intOf(cur)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func cloneMap(in map[string]any) map[string]any {
	out := map[string]any{}
	if in == nil {
		return out
	}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *Service) writeTaskLog(ctx context.Context, source string, result map[string]any) {
	if s == nil || s.Store == nil || result == nil {
		return
	}
	// Skip pure lock-busy no-ops.
	if result["deferred_busy"] == true {
		return
	}
	probed := intOf(result["probed"])
	count := intOf(result["count"])
	if count == 0 {
		count = probed
	}
	available := intOf(result["available_count"])
	if available == 0 {
		available = intOf(result["available"])
	}
	if probed == 0 && count == 0 && source == "background" {
		return
	}
	okVal := result["ok"] != false
	status := "done"
	if result["ok"] == false {
		status = "error"
		okVal = false
	} else if result["budget_hit"] == true || intOf(result["deferred"]) > 0 || intOf(result["unavailable_count"]) > 0 {
		// partial when some accounts failed/deferred but overall job completed
		if result["ok"] != false {
			status = "partial"
		}
	}
	// Prefer account-level progress for multi-wave (probed accounts), fall back to probe count.
	progressTotal := probed
	if progressTotal == 0 {
		progressTotal = count
	}
	progressDone := available
	if progressDone > progressTotal && progressTotal > 0 {
		// available is per-probe; clamp display to total accounts when multi-model.
		// Keep raw numbers in detail.
	}
	summary := "模型探测[" + source + "]：可用 " + itoa(available) + "/" + itoa(progressTotal)
	if count > 0 && count != progressTotal {
		summary += "（探测 " + itoa(count) + " 次）"
	}
	if intOf(result["kick_cooldown"])+intOf(result["kick_disabled"]) > 0 {
		summary += " · 冷却踢出 " + itoa(intOf(result["kick_cooldown"])) + " · 禁用 " + itoa(intOf(result["kick_disabled"]))
	}
	if intOf(result["model_blocked_count"]) > 0 {
		summary += " · 模型封禁 " + itoa(intOf(result["model_blocked_count"]))
	}
	if intOf(result["waves"]) > 0 {
		summary += " · 波次 " + itoa(intOf(result["waves"]))
	}
	if intOf(result["recovered"]) > 0 {
		summary += " · 恢复 " + itoa(intOf(result["recovered"]))
	}
	detail := map[string]any{}
	for _, k := range []string{
		"probed", "count", "available", "available_count", "unavailable_count", "failed",
		"kick_cooldown", "kick_disabled", "model_blocked_count", "recovered",
		"workers", "waves", "budget_hit", "models", "source", "elapsed_ms", "sweep",
		"failed_sample", "ok",
	} {
		if v, ok := result[k]; ok {
			detail[k] = v
		}
	}
	// Stable task_id so multi-wave / progress updates merge into one row when job id is known.
	taskID := "probe:" + source
	if jid, ok := result["job_id"].(string); ok && strings.TrimSpace(jid) != "" {
		taskID = "probe:" + strings.TrimSpace(jid)
	} else if jid, ok := result["task_id"].(string); ok && strings.TrimSpace(jid) != "" {
		taskID = strings.TrimSpace(jid)
	}
	// For background cycles, keep one rolling row per day+source so UI isn't flooded,
	// but still update the same row within the day.
	if source == "background" {
		taskID = "probe:background:" + time.Now().Format("2006-01-02")
	}
	finished := true
	if result["running"] == true || status == "running" || status == "queued" {
		finished = false
		if status == "done" {
			status = "running"
		}
	}
	okPtr := okVal
	_, _ = s.Store.WriteTask(ctx, "probe", status, summary, taskID, &okPtr, detail, progressDone, progressTotal, finished)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func isFreeUsageExhausted(errText string) bool {
	return pool.IsFreeUsageExhausted(errText)
}

func firstModel(models []string) string {
	if len(models) == 0 {
		return "gpt-6.1-sol"
	}
	return models[0]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func normalizeModels(models []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(models))
	for _, m := range models {
		mid := strings.TrimSpace(m)
		if mid == "" {
			continue
		}
		key := strings.ToLower(mid)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, mid)
	}
	return out
}

func splitCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func envDurationSec(name string, fallback, min, max time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	sec, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	d := time.Duration(sec * float64(time.Second))
	if d < min {
		return min
	}
	if d > max {
		return max
	}
	return d
}

func envInt(name string, fallback, min, max int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
