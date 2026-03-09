package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"ai-gateway/internal/balancer"
	"ai-gateway/internal/upstream"
)

// Proxy handles forwarding requests to upstream AI providers.
type Proxy struct {
	balancer   *balancer.Balancer
	maxRetries int
	timeout    time.Duration
	client     *http.Client
	logger     *slog.Logger
}

// New creates a Proxy.
func New(b *balancer.Balancer, maxRetries int, timeout time.Duration, logger *slog.Logger) *Proxy {
	return &Proxy{
		balancer:   b,
		maxRetries: maxRetries,
		timeout:    timeout,
		client: &http.Client{
			Timeout: 0, // we handle timeout per-request for streaming support
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logger: logger,
	}
}

// chatRequest is the minimal structure we parse to extract the model name.
type chatRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// Handler returns an http.HandlerFunc for proxying API requests.
func (p *Proxy) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.handle(w, r)
	}
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// Determine the model from the request
	model, bodyBytes, isStreaming, err := p.extractModel(r)
	if err != nil {
		p.logger.Error("failed to extract model", "error", err, "path", r.URL.Path)
		// Continue without model filter — will try any upstream
	}

	// Try upstreams with retries
	var lastErr error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		u := p.balancer.Select(model)
		if u == nil {
			p.logger.Error("no available upstream",
				"model", model, "attempt", attempt)
			http.Error(w, `{"error":{"message":"no available upstream","type":"gateway_error"}}`,
				http.StatusBadGateway)
			return
		}

		if !u.TryAcquire() {
			p.logger.Warn("upstream rejected by circuit breaker",
				"upstream", u.Name, "attempt", attempt)
			lastErr = fmt.Errorf("circuit breaker open for %s", u.Name)
			continue
		}

		reqStart := time.Now()
		err := p.forwardRequest(w, r, u, bodyBytes, isStreaming)
		latency := time.Since(reqStart)

		if err == nil {
			u.Release(true, latency)
			p.logger.Info("request proxied",
				"upstream", u.Name,
				"model", model,
				"path", r.URL.Path,
				"latency", latency,
				"total_time", time.Since(startTime),
			)
			return
		}

		u.Release(false, latency)
		lastErr = err
		p.logger.Warn("upstream request failed, retrying",
			"upstream", u.Name,
			"error", err,
			"attempt", attempt,
			"latency", latency,
		)
	}

	p.logger.Error("all retries exhausted",
		"model", model,
		"error", lastErr,
		"total_time", time.Since(startTime),
	)
	http.Error(w, `{"error":{"message":"all upstreams failed","type":"gateway_error"}}`,
		http.StatusBadGateway)
}

func (p *Proxy) extractModel(r *http.Request) (model string, bodyBytes []byte, isStreaming bool, err error) {
	// For GET requests (like /v1/models), no body to parse
	if r.Method == http.MethodGet {
		return "", nil, false, nil
	}

	if r.Body == nil {
		return "", nil, false, nil
	}

	bodyBytes, err = io.ReadAll(r.Body)
	if err != nil {
		return "", nil, false, fmt.Errorf("read request body: %w", err)
	}
	r.Body.Close()

	var req chatRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		return "", bodyBytes, false, nil // body exists but not JSON we understand
	}

	return req.Model, bodyBytes, req.Stream, nil
}

func (p *Proxy) forwardRequest(w http.ResponseWriter, r *http.Request, u *upstream.Upstream, bodyBytes []byte, isStreaming bool) error {
	// Build upstream URL
	targetURL := u.BaseURL + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	// Create upstream request
	var body io.Reader
	if bodyBytes != nil {
		body = strings.NewReader(string(bodyBytes))
	}

	ctx := r.Context()
	// For non-streaming, apply a timeout
	if !isStreaming && p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	upReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, body)
	if err != nil {
		return fmt.Errorf("create upstream request: %w", err)
	}

	// Copy headers, replace Authorization
	for key, vals := range r.Header {
		if strings.EqualFold(key, "Authorization") {
			continue
		}
		for _, v := range vals {
			upReq.Header.Add(key, v)
		}
	}
	upReq.Header.Set("Authorization", "Bearer "+u.APIKey)

	if bodyBytes != nil && upReq.Header.Get("Content-Type") == "" {
		upReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.client.Do(upReq)
	if err != nil {
		return fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	// Treat 5xx as retryable failures
	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	// Copy response headers
	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}

	// For SSE streaming, flush incrementally
	if isStreaming && isSSEResponse(resp) {
		w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
		w.WriteHeader(resp.StatusCode)

		flusher, ok := w.(http.Flusher)
		if !ok {
			_, err := io.Copy(w, resp.Body)
			return err
		}

		buf := make([]byte, 4096)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					return writeErr
				}
				flusher.Flush()
			}
			if readErr != nil {
				if readErr == io.EOF {
					return nil
				}
				return readErr
			}
		}
	}

	// Non-streaming: simple copy
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	return err
}

func isSSEResponse(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(ct, "text/event-stream")
}
