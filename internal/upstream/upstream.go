package upstream

import (
	"sync"
	"sync/atomic"
	"time"

	"ai-gateway/internal/config"
)

// CircuitState represents the state of a circuit breaker.
type CircuitState int

const (
	CircuitClosed   CircuitState = iota // normal operation
	CircuitOpen                         // failing, reject requests
	CircuitHalfOpen                     // testing recovery
)

func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Upstream represents a single backend AI API provider.
type Upstream struct {
	Name    string
	BaseURL string
	APIKey  string
	Weight  int
	Models  map[string]bool

	mu                sync.RWMutex
	healthy           bool
	enabled           bool
	activeConnections int64
	consecutiveFails  int
	lastFailure       time.Time
	lastLatency       time.Duration
	totalRequests     int64
	totalFailures     int64

	// Circuit breaker
	cbState            CircuitState
	cbFailures         int
	cbLastStateChange  time.Time
	cbFailureThreshold int
	cbRecoveryTimeout  time.Duration

	// Health check
	hcFailureThreshold int
	hcRecoveryCooldown time.Duration
}

// Status holds a snapshot of upstream state for external consumption.
type Status struct {
	Name              string        `json:"name"`
	BaseURL           string        `json:"base_url"`
	Healthy           bool          `json:"healthy"`
	Enabled           bool          `json:"enabled"`
	ActiveConnections int64         `json:"active_connections"`
	ConsecutiveFails  int           `json:"consecutive_failures"`
	LastLatency       string        `json:"last_latency"`
	TotalRequests     int64         `json:"total_requests"`
	TotalFailures     int64         `json:"total_failures"`
	CircuitState      string        `json:"circuit_state"`
	Models            []string      `json:"models"`
}

// New creates an Upstream from config.
func New(cfg config.UpstreamConfig, cbCfg config.CircuitBreakerConfig, hcCfg config.HealthCheckConfig) *Upstream {
	models := make(map[string]bool, len(cfg.Models))
	for _, m := range cfg.Models {
		models[m] = true
	}
	return &Upstream{
		Name:               cfg.Name,
		BaseURL:            cfg.BaseURL,
		APIKey:             cfg.APIKey,
		Weight:             cfg.Weight,
		Models:             models,
		healthy:            true,
		enabled:            true,
		cbState:            CircuitClosed,
		cbFailureThreshold: cbCfg.FailureThreshold,
		cbRecoveryTimeout:  cbCfg.RecoveryTimeout,
		hcFailureThreshold: hcCfg.FailureThreshold,
		hcRecoveryCooldown: hcCfg.RecoveryCooldown,
	}
}

// SupportsModel checks if this upstream serves the given model.
func (u *Upstream) SupportsModel(model string) bool {
	if len(u.Models) == 0 {
		return true // no model filter means supports all
	}
	return u.Models[model]
}

// IsAvailable returns true if the upstream can accept requests.
func (u *Upstream) IsAvailable() bool {
	u.mu.RLock()
	defer u.mu.RUnlock()

	if !u.enabled || !u.healthy {
		return false
	}

	switch u.cbState {
	case CircuitOpen:
		if time.Since(u.cbLastStateChange) > u.cbRecoveryTimeout {
			return true // will transition to half-open on next attempt
		}
		return false
	default:
		return true
	}
}

// TryAcquire attempts to acquire a connection slot.
// Returns false if the circuit breaker rejects the request.
func (u *Upstream) TryAcquire() bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	if !u.enabled || !u.healthy {
		return false
	}

	switch u.cbState {
	case CircuitOpen:
		if time.Since(u.cbLastStateChange) > u.cbRecoveryTimeout {
			u.cbState = CircuitHalfOpen
			u.cbLastStateChange = time.Now()
			// allow one probe request
		} else {
			return false
		}
	case CircuitHalfOpen:
		// only allow one request at a time in half-open
		if u.activeConnections > 0 {
			return false
		}
	}

	atomic.AddInt64(&u.activeConnections, 1)
	atomic.AddInt64(&u.totalRequests, 1)
	return true
}

// Release releases a connection slot and records the result.
func (u *Upstream) Release(success bool, latency time.Duration) {
	atomic.AddInt64(&u.activeConnections, -1)

	u.mu.Lock()
	defer u.mu.Unlock()

	u.lastLatency = latency

	if success {
		u.consecutiveFails = 0
		u.cbFailures = 0
		if u.cbState == CircuitHalfOpen {
			u.cbState = CircuitClosed
			u.cbLastStateChange = time.Now()
		}
	} else {
		atomic.AddInt64(&u.totalFailures, 1)
		u.consecutiveFails++
		u.lastFailure = time.Now()
		u.cbFailures++

		if u.cbFailures >= u.cbFailureThreshold {
			u.cbState = CircuitOpen
			u.cbLastStateChange = time.Now()
			u.cbFailures = 0
		}
	}
}

// ActiveConns returns the current number of active connections.
func (u *Upstream) ActiveConns() int64 {
	return atomic.LoadInt64(&u.activeConnections)
}

// SetHealthy updates the health status from health checker.
func (u *Upstream) SetHealthy(healthy bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.healthy = healthy
}

// SetEnabled allows manual enable/disable.
func (u *Upstream) SetEnabled(enabled bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.enabled = enabled
	if enabled {
		// reset circuit breaker on manual enable
		u.cbState = CircuitClosed
		u.cbFailures = 0
		u.consecutiveFails = 0
	}
}

// IsEnabled returns whether the upstream is manually enabled.
func (u *Upstream) IsEnabled() bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.enabled
}

// IsHealthy returns whether the upstream is considered healthy.
func (u *Upstream) IsHealthy() bool {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.healthy
}

// RecordHealthCheck records the result of a health probe.
func (u *Upstream) RecordHealthCheck(success bool, latency time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.lastLatency = latency

	if success {
		u.consecutiveFails = 0
		u.healthy = true
	} else {
		u.consecutiveFails++
		if u.consecutiveFails >= u.hcFailureThreshold {
			u.healthy = false
		}
	}
}

// GetStatus returns a snapshot of this upstream's state.
func (u *Upstream) GetStatus() Status {
	u.mu.RLock()
	defer u.mu.RUnlock()

	models := make([]string, 0, len(u.Models))
	for m := range u.Models {
		models = append(models, m)
	}

	return Status{
		Name:              u.Name,
		BaseURL:           u.BaseURL,
		Healthy:           u.healthy,
		Enabled:           u.enabled,
		ActiveConnections: atomic.LoadInt64(&u.activeConnections),
		ConsecutiveFails:  u.consecutiveFails,
		LastLatency:       u.lastLatency.String(),
		TotalRequests:     atomic.LoadInt64(&u.totalRequests),
		TotalFailures:     atomic.LoadInt64(&u.totalFailures),
		CircuitState:      u.cbState.String(),
		Models:            models,
	}
}
