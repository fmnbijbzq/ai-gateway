package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"ai-gateway/internal/upstream"
)

// Checker performs periodic health probes on all upstreams.
type Checker struct {
	upstreams []*upstream.Upstream
	interval  time.Duration
	timeout   time.Duration
	client    *http.Client
	logger    *slog.Logger
}

// New creates a health Checker.
func New(upstreams []*upstream.Upstream, interval, timeout time.Duration, logger *slog.Logger) *Checker {
	return &Checker{
		upstreams: upstreams,
		interval:  interval,
		timeout:   timeout,
		client: &http.Client{
			Timeout: timeout,
		},
		logger: logger,
	}
}

// Start begins periodic health checking. Blocks until ctx is cancelled.
func (c *Checker) Start(ctx context.Context) {
	c.logger.Info("health checker started", "interval", c.interval)

	// Run an immediate check at startup
	c.checkAll(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("health checker stopped")
			return
		case <-ticker.C:
			c.checkAll(ctx)
		}
	}
}

func (c *Checker) checkAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, u := range c.upstreams {
		if !u.IsEnabled() {
			continue
		}
		wg.Add(1)
		go func(u *upstream.Upstream) {
			defer wg.Done()
			c.checkOne(ctx, u)
		}(u)
	}
	wg.Wait()
}

func (c *Checker) checkOne(ctx context.Context, u *upstream.Upstream) {
	baseURL := strings.TrimRight(u.BaseURL, "/")
	healthURL := baseURL + "/v1/models"
	if strings.HasSuffix(baseURL, "/v1") {
		healthURL = baseURL + "/models"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		c.logger.Error("create health check request failed",
			"upstream", u.Name, "error", err)
		u.RecordHealthCheck(false, 0)
		return
	}

	// Use correct auth header based on API type
	if u.APIType == "anthropic" {
		req.Header.Set("x-api-key", u.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+u.APIKey)
	}

	start := time.Now()
	resp, err := c.client.Do(req)
	latency := time.Since(start)

	if err != nil {
		c.logger.Warn("health check failed",
			"upstream", u.Name, "error", err, "latency", latency)
		u.RecordHealthCheck(false, latency)
		return
	}
	resp.Body.Close()

	success := resp.StatusCode >= 200 && resp.StatusCode < 500
	if success {
		c.logger.Debug("health check passed",
			"upstream", u.Name, "status", resp.StatusCode, "latency", latency)
	} else {
		c.logger.Warn("health check unhealthy status",
			"upstream", u.Name, "status", resp.StatusCode, "latency", latency)
	}
	u.RecordHealthCheck(success, latency)
}

// HealthResponse is the JSON structure for /health.
type HealthResponse struct {
	Status    string            `json:"status"`
	Upstreams []upstream.Status `json:"upstreams"`
}

// Handler returns an http.HandlerFunc for the /health endpoint.
func (c *Checker) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := HealthResponse{
			Status:    "ok",
			Upstreams: make([]upstream.Status, 0, len(c.upstreams)),
		}

		allHealthy := true
		for _, u := range c.upstreams {
			st := u.GetStatus()
			resp.Upstreams = append(resp.Upstreams, st)
			if !st.Healthy || !st.Enabled {
				allHealthy = false
			}
		}

		if !allHealthy {
			resp.Status = "degraded"
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}
