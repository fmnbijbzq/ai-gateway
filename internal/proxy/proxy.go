package proxy

import (
	"bufio"
	"bytes"
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
			Timeout: 0,
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

// Handler returns an http.HandlerFunc for proxying OpenAI-format API requests (/v1/chat/completions etc).
func (p *Proxy) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.handle(w, r)
	}
}

// MessagesHandler returns an http.HandlerFunc for the Anthropic Messages API entry point (/v1/messages).
func (p *Proxy) MessagesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		p.handleMessages(w, r)
	}
}

// ModelsHandler returns an http.HandlerFunc for GET /v1/models.
func (p *Proxy) ModelsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		p.handleModels(w, r)
	}
}

func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	model, bodyBytes, isStreaming, err := p.extractModel(r)
	if err != nil {
		p.logger.Error("failed to extract model", "error", err, "path", r.URL.Path)
	}

	var lastErr error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		u := p.balancer.Select(model)
		if u == nil {
			p.logger.Error("no available upstream", "model", model, "attempt", attempt)
			http.Error(w, `{"error":{"message":"no available upstream","type":"gateway_error"}}`,
				http.StatusBadGateway)
			return
		}

		if !u.TryAcquire() {
			p.logger.Warn("upstream rejected by circuit breaker", "upstream", u.Name, "attempt", attempt)
			lastErr = fmt.Errorf("circuit breaker open for %s", u.Name)
			continue
		}

		reqStart := time.Now()
		err := p.forwardRequest(w, r, u, bodyBytes, isStreaming)
		latency := time.Since(reqStart)

		if err == nil {
			u.Release(true, latency)
			p.logger.Info("request proxied",
				"upstream", u.Name, "model", model, "path", r.URL.Path,
				"latency", latency, "total_time", time.Since(startTime))
			return
		}

		u.Release(false, latency)
		lastErr = err
		p.logger.Warn("upstream request failed, retrying",
			"upstream", u.Name, "error", err, "attempt", attempt, "latency", latency)
	}

	p.logger.Error("all retries exhausted", "model", model, "error", lastErr, "total_time", time.Since(startTime))
	http.Error(w, `{"error":{"message":"all upstreams failed","type":"gateway_error"}}`,
		http.StatusBadGateway)
}

// handleMessages handles Anthropic Messages format requests.
// If upstream is anthropic: passthrough. If openai: convert request and response.
func (p *Proxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	r.Body.Close()

	var req chatRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	model := req.Model
	isStreaming := req.Stream

	var lastErr error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		u := p.balancer.Select(model)
		if u == nil {
			writeJSONError(w, http.StatusBadGateway, "no available upstream")
			return
		}

		if !u.TryAcquire() {
			lastErr = fmt.Errorf("circuit breaker open for %s", u.Name)
			continue
		}

		reqStart := time.Now()
		var fwdErr error
		if u.APIType == "anthropic" {
			fwdErr = p.forwardAnthropicPassthrough(w, r, u, bodyBytes, isStreaming)
		} else {
			fwdErr = p.forwardAnthropicToOpenAI(w, r, u, bodyBytes, isStreaming)
		}
		latency := time.Since(reqStart)

		if fwdErr == nil {
			u.Release(true, latency)
			p.logger.Info("messages request proxied",
				"upstream", u.Name, "model", model, "api_type", u.APIType,
				"latency", latency, "total_time", time.Since(startTime))
			return
		}

		u.Release(false, latency)
		lastErr = fwdErr
		p.logger.Warn("messages upstream failed, retrying",
			"upstream", u.Name, "error", fwdErr, "attempt", attempt, "latency", latency)
	}

	p.logger.Error("messages all retries exhausted", "model", model, "error", lastErr)
	writeJSONError(w, http.StatusBadGateway, "all upstreams failed")
}

// forwardAnthropicPassthrough forwards an Anthropic-format request to an Anthropic upstream as-is.
func (p *Proxy) forwardAnthropicPassthrough(w http.ResponseWriter, r *http.Request, u *upstream.Upstream, bodyBytes []byte, isStreaming bool) error {
	baseURL := strings.TrimRight(u.BaseURL, "/")
	// Fix double /v1: if base_url already ends with /v1, use /messages directly
	targetURL := baseURL + "/v1/messages"
	if strings.HasSuffix(baseURL, "/v1") {
		targetURL = baseURL + "/messages"
	}

	ctx := r.Context()
	if !isStreaming && p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create upstream request: %w", err)
	}

	// Set Anthropic auth headers
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("x-api-key", u.APIKey)

	// Pass through anthropic-version (use client's or default)
	if v := r.Header.Get("anthropic-version"); v != "" {
		upReq.Header.Set("anthropic-version", v)
	} else {
		upReq.Header.Set("anthropic-version", "2023-06-01")
	}

	// Pass through anthropic-beta for prompt caching
	if beta := r.Header.Get("anthropic-beta"); beta != "" {
		upReq.Header.Set("anthropic-beta", beta)
	}

	resp, err := p.client.Do(upReq)
	if err != nil {
		return fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

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

	if isStreaming && isSSEResponse(resp) {
		return p.streamPassthrough(w, resp)
	}

	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	return err
}

// forwardAnthropicToOpenAI converts Anthropic Messages request to OpenAI format, sends to OpenAI upstream,
// and converts the response back to Anthropic format.
func (p *Proxy) forwardAnthropicToOpenAI(w http.ResponseWriter, r *http.Request, u *upstream.Upstream, bodyBytes []byte, isStreaming bool) error {
	// Convert Anthropic request to OpenAI format
	openaiBody, err := convertAnthropicToOpenAI(bodyBytes)
	if err != nil {
		return fmt.Errorf("convert request: %w", err)
	}

	baseURL := strings.TrimRight(u.BaseURL, "/")
	targetURL := baseURL + "/v1/chat/completions"
	if strings.HasSuffix(baseURL, "/v1") {
		targetURL = baseURL + "/chat/completions"
	}

	ctx := r.Context()
	if !isStreaming && p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(openaiBody))
	if err != nil {
		return fmt.Errorf("create upstream request: %w", err)
	}

	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+u.APIKey)

	resp, err := p.client.Do(upReq)
	if err != nil {
		return fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	// If upstream returned an error (4xx), convert to Anthropic error format
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return nil
	}

	if isStreaming && isSSEResponse(resp) {
		return p.streamOpenAIToAnthropic(w, resp)
	}

	// Non-streaming: read OpenAI response and convert to Anthropic format
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read upstream response: %w", err)
	}

	anthropicResp, err := convertOpenAIResponseToAnthropic(respBody)
	if err != nil {
		// Fallback: return raw response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return nil
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(anthropicResp)
	return nil
}

// handleModels returns a combined list of all models from all upstreams.
func (p *Proxy) handleModels(w http.ResponseWriter, _ *http.Request) {
	seen := make(map[string]bool)
	var models []map[string]interface{}

	for _, u := range p.balancer.Upstreams() {
		if !u.IsAvailable() {
			continue
		}
		for m := range u.Models {
			if seen[m] {
				continue
			}
			seen[m] = true
			models = append(models, map[string]interface{}{
				"id":   m,
				"name": m,
				"type": "model",
			})
		}
	}

	resp := map[string]interface{}{
		"data": models,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (p *Proxy) extractModel(r *http.Request) (model string, bodyBytes []byte, isStreaming bool, err error) {
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
		return "", bodyBytes, false, nil
	}
	return req.Model, bodyBytes, req.Stream, nil
}

func (p *Proxy) forwardRequest(w http.ResponseWriter, r *http.Request, u *upstream.Upstream, bodyBytes []byte, isStreaming bool) error {
	path := r.URL.Path
	baseURL := strings.TrimRight(u.BaseURL, "/")
	if strings.HasSuffix(baseURL, "/v1") && strings.HasPrefix(path, "/v1") {
		path = strings.TrimPrefix(path, "/v1")
	}
	targetURL := baseURL + path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	var body io.Reader
	if u.APIType == "anthropic" && bodyBytes != nil {
		convertedBody, convertedStreaming := p.convertToAnthropic(bodyBytes)
		if convertedBody != nil {
			bodyBytes = convertedBody
			if convertedStreaming {
				isStreaming = true
			}
		}
		targetURL = strings.Replace(targetURL, "/chat/completions", "/messages", 1)
		body = bytes.NewReader(bodyBytes)
	} else if bodyBytes != nil {
		body = bytes.NewReader(bodyBytes)
	}

	ctx := r.Context()
	if !isStreaming && p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	upReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, body)
	if err != nil {
		return fmt.Errorf("create upstream request: %w", err)
	}

	for key, vals := range r.Header {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "X-Api-Key") {
			continue
		}
		for _, v := range vals {
			upReq.Header.Add(key, v)
		}
	}

	if u.APIType == "anthropic" {
		upReq.Header.Set("x-api-key", u.APIKey)
		if v := r.Header.Get("anthropic-version"); v != "" {
			upReq.Header.Set("anthropic-version", v)
		} else {
			upReq.Header.Set("anthropic-version", "2023-06-01")
		}
		if beta := r.Header.Get("anthropic-beta"); beta != "" {
			upReq.Header.Set("anthropic-beta", beta)
		}
		upReq.Header.Del("Authorization")
	} else {
		upReq.Header.Set("Authorization", "Bearer "+u.APIKey)
	}

	if bodyBytes != nil && upReq.Header.Get("Content-Type") == "" {
		upReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := p.client.Do(upReq)
	if err != nil {
		return fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	for key, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}

	if isStreaming && isSSEResponse(resp) {
		return p.streamPassthrough(w, resp)
	}

	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	return err
}

// streamPassthrough streams SSE data through without conversion.
func (p *Proxy) streamPassthrough(w http.ResponseWriter, resp *http.Response) error {
	w.Header().Set("X-Accel-Buffering", "no")
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

// streamOpenAIToAnthropic reads OpenAI SSE stream and converts each chunk to Anthropic SSE format.
func (p *Proxy) streamOpenAIToAnthropic(w http.ResponseWriter, resp *http.Response) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		_, err := io.Copy(w, resp.Body)
		return err
	}

	// Send message_start event
	msgID := "msg_" + fmt.Sprintf("%d", time.Now().UnixNano())
	model := ""
	messageStart := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":      msgID,
			"type":    "message",
			"role":    "assistant",
			"content": []interface{}{},
			"model":   "",
			"usage":   map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
		},
	}
	writeSSEEvent(w, "message_start", messageStart)
	flusher.Flush()

	// Send content_block_start
	blockStart := map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	}
	writeSSEEvent(w, "content_block_start", blockStart)
	flusher.Flush()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var totalOutputTokens int
	var inputTokens int

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		// Extract model from first chunk
		if model == "" {
			if m, ok := chunk["model"].(string); ok {
				model = m
			}
		}

		// Extract usage if present
		if usage, ok := chunk["usage"].(map[string]interface{}); ok {
			if pt, ok := usage["prompt_tokens"].(float64); ok {
				inputTokens = int(pt)
			}
			if ct, ok := usage["completion_tokens"].(float64); ok {
				totalOutputTokens = int(ct)
			}
		}

		// Process choices
		choices, ok := chunk["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}

		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			continue
		}

		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			continue
		}

		if content, ok := delta["content"].(string); ok && content != "" {
			totalOutputTokens++
			blockDelta := map[string]interface{}{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": content,
				},
			}
			writeSSEEvent(w, "content_block_delta", blockDelta)
			flusher.Flush()
		}

		// Check for finish_reason
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			// content_block_stop
			blockStop := map[string]interface{}{
				"type":  "content_block_stop",
				"index": 0,
			}
			writeSSEEvent(w, "content_block_stop", blockStop)
			flusher.Flush()

			// message_delta with stop reason
			stopReason := convertFinishReason(fr)
			msgDelta := map[string]interface{}{
				"type": "message_delta",
				"delta": map[string]interface{}{
					"stop_reason": stopReason,
				},
				"usage": map[string]interface{}{
					"output_tokens": totalOutputTokens,
				},
			}
			writeSSEEvent(w, "message_delta", msgDelta)
			flusher.Flush()
		}
	}

	// Send message_stop
	msgStop := map[string]interface{}{
		"type": "message_stop",
	}
	writeSSEEvent(w, "message_stop", msgStop)
	flusher.Flush()

	// Update message_start model if we got it (already sent, but useful for logs)
	_ = model
	_ = inputTokens

	return scanner.Err()
}

// convertAnthropicToOpenAI converts an Anthropic Messages API request body to OpenAI chat completion format.
func convertAnthropicToOpenAI(body []byte) ([]byte, error) {
	var anthropicReq map[string]interface{}
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		return nil, err
	}

	openaiReq := map[string]interface{}{}

	if model, ok := anthropicReq["model"]; ok {
		openaiReq["model"] = model
	}

	var openaiMessages []interface{}

	// Convert system to a system message
	if system, ok := anthropicReq["system"]; ok {
		switch s := system.(type) {
		case string:
			if s != "" {
				openaiMessages = append(openaiMessages, map[string]interface{}{
					"role":    "system",
					"content": s,
				})
			}
		case []interface{}:
			// Anthropic system can be array of content blocks
			var parts []string
			for _, block := range s {
				if b, ok := block.(map[string]interface{}); ok {
					if text, ok := b["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
			if len(parts) > 0 {
				openaiMessages = append(openaiMessages, map[string]interface{}{
					"role":    "system",
					"content": strings.Join(parts, "\n"),
				})
			}
		}
	}

	// Convert messages
	if messages, ok := anthropicReq["messages"].([]interface{}); ok {
		for _, msg := range messages {
			m, ok := msg.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := m["role"].(string)

			// Handle content which may be string or array of content blocks
			switch content := m["content"].(type) {
			case string:
				openaiMessages = append(openaiMessages, map[string]interface{}{
					"role":    role,
					"content": content,
				})
			case []interface{}:
				// Extract text from content blocks
				var textParts []string
				for _, block := range content {
					if b, ok := block.(map[string]interface{}); ok {
						if b["type"] == "text" {
							if text, ok := b["text"].(string); ok {
								textParts = append(textParts, text)
							}
						}
					}
				}
				if len(textParts) > 0 {
					openaiMessages = append(openaiMessages, map[string]interface{}{
						"role":    role,
						"content": strings.Join(textParts, ""),
					})
				}
			}
		}
	}

	openaiReq["messages"] = openaiMessages

	if maxTokens, ok := anthropicReq["max_tokens"]; ok {
		openaiReq["max_tokens"] = maxTokens
	}
	if temp, ok := anthropicReq["temperature"]; ok {
		openaiReq["temperature"] = temp
	}
	if topP, ok := anthropicReq["top_p"]; ok {
		openaiReq["top_p"] = topP
	}
	if stream, ok := anthropicReq["stream"]; ok {
		openaiReq["stream"] = stream
		// Request usage in streaming for token counts
		if s, ok := stream.(bool); ok && s {
			openaiReq["stream_options"] = map[string]interface{}{
				"include_usage": true,
			}
		}
	}
	if stop, ok := anthropicReq["stop_sequences"]; ok {
		openaiReq["stop"] = stop
	}

	return json.Marshal(openaiReq)
}

// convertOpenAIResponseToAnthropic converts a non-streaming OpenAI chat completion response to Anthropic Messages format.
func convertOpenAIResponseToAnthropic(body []byte) ([]byte, error) {
	var openaiResp map[string]interface{}
	if err := json.Unmarshal(body, &openaiResp); err != nil {
		return nil, err
	}

	id, _ := openaiResp["id"].(string)
	model, _ := openaiResp["model"].(string)

	// Extract content from first choice
	content := ""
	stopReason := "end_turn"
	if choices, ok := openaiResp["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := choice["message"].(map[string]interface{}); ok {
				content, _ = msg["content"].(string)
			}
			if fr, ok := choice["finish_reason"].(string); ok {
				stopReason = convertFinishReason(fr)
			}
		}
	}

	// Extract usage
	inputTokens := 0
	outputTokens := 0
	if usage, ok := openaiResp["usage"].(map[string]interface{}); ok {
		if pt, ok := usage["prompt_tokens"].(float64); ok {
			inputTokens = int(pt)
		}
		if ct, ok := usage["completion_tokens"].(float64); ok {
			outputTokens = int(ct)
		}
	}

	anthropicResp := map[string]interface{}{
		"id":   "msg_" + id,
		"type": "message",
		"role": "assistant",
		"content": []map[string]interface{}{
			{"type": "text", "text": content},
		},
		"model":       model,
		"stop_reason": stopReason,
		"usage": map[string]interface{}{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}

	return json.Marshal(anthropicResp)
}

// convertToAnthropic converts OpenAI chat completion request to Anthropic Messages format.
// Used by the /v1/chat/completions passthrough path.
func (p *Proxy) convertToAnthropic(bodyBytes []byte) ([]byte, bool) {
	var openaiReq map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &openaiReq); err != nil {
		return nil, false
	}

	anthropicReq := make(map[string]interface{})

	if model, ok := openaiReq["model"]; ok {
		anthropicReq["model"] = model
	}

	if messages, ok := openaiReq["messages"].([]interface{}); ok {
		var systemParts []string
		var userMessages []interface{}
		for _, msg := range messages {
			m, ok := msg.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			content, _ := m["content"].(string)
			if role == "system" {
				systemParts = append(systemParts, content)
			} else {
				userMessages = append(userMessages, m)
			}
		}
		if len(systemParts) > 0 {
			anthropicReq["system"] = strings.Join(systemParts, "\n")
		}
		anthropicReq["messages"] = userMessages
	}

	if maxTokens, ok := openaiReq["max_tokens"]; ok {
		anthropicReq["max_tokens"] = maxTokens
	} else {
		anthropicReq["max_tokens"] = 4096
	}

	streaming := false
	if stream, ok := openaiReq["stream"].(bool); ok {
		anthropicReq["stream"] = stream
		streaming = stream
	}
	if temp, ok := openaiReq["temperature"]; ok {
		anthropicReq["temperature"] = temp
	}
	if topP, ok := openaiReq["top_p"]; ok {
		anthropicReq["top_p"] = topP
	}

	result, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, false
	}
	return result, streaming
}

func convertFinishReason(fr string) string {
	switch fr {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func writeSSEEvent(w http.ResponseWriter, eventType string, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(jsonData))
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "gateway_error",
			"message": message,
		},
	}
	json.NewEncoder(w).Encode(resp)
}

func isSSEResponse(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(ct, "text/event-stream")
}
