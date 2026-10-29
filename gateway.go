package main

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBody = 10 << 20

type Gateway struct {
	cfg     *Config
	client  *http.Client
	limits  *limiter
	metrics *metrics
	cache   *cache // nil when caching is off
}

func NewGateway(cfg *Config) (*Gateway, error) {
	g := &Gateway{
		cfg: cfg,
		// No overall timeout: a stream can run for minutes. Waiting for the first byte is bounded instead.
		client:  &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 5 * time.Minute}},
		limits:  &limiter{tat: map[string]time.Time{}, now: time.Now},
		metrics: newMetrics(),
	}
	if cfg.Cache != nil {
		c, err := newCache(cfg.Cache)
		if err != nil {
			return nil, err
		}
		g.cache = c
	}
	return g, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.chat)
	mux.HandleFunc("GET /v1/models", g.models)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, g.metrics.text())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	return mux
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request) {
	client, ok := g.authenticate(r)
	if !ok {
		apiError(w, http.StatusUnauthorized, "invalid or missing API key")
		return
	}
	if wait, ok := g.limits.allow(client, g.cfg.Keys[client].RPM); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		apiError(w, http.StatusTooManyRequests, fmt.Sprintf("rate limit for %q exceeded, retry in %s", client, wait.Round(time.Millisecond)))
		return
	}

	// A generic map keeps every field the client sent (tools, response_format, ...) untouched.
	var req map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "request body is not valid JSON: "+err.Error())
		return
	}
	alias, _ := req["model"].(string)
	routes, ok := g.cfg.Models[alias]
	if !ok {
		apiError(w, http.StatusNotFound, fmt.Sprintf("unknown model %q; available: %s", alias, strings.Join(g.cfg.aliases(), ", ")))
		return
	}
	stream, _ := req["stream"].(bool)
	if stream {
		// Ask for the final usage chunk so streamed requests are metered too.
		opts, _ := req["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		req["stream_options"] = opts
	}

	key := ""
	if g.cache != nil {
		if key = cacheKey(client, req, stream); key != "" {
			if e, ok := g.cache.get(key); ok {
				w.Header().Set("Content-Type", e.contentType)
				w.Header().Set("X-Gateway-Upstream", e.upstream)
				w.Header().Set("X-Gateway-Cache", "hit")
				_, _ = w.Write(e.body)
				g.metrics.request(client, alias, e.upstream, "cache_hit")
				return
			}
			w.Header().Set("X-Gateway-Cache", "miss")
		}
	}

	var failures []string
	for _, route := range routes {
		req["model"] = route.Model
		payload, _ := json.Marshal(req)
		start := time.Now()
		resp, err := g.send(r, route, payload)
		if err == nil && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			defer func() { _ = resp.Body.Close() }()
			body := g.relay(w, resp, stream, record{client: client, alias: alias, route: route, start: start})
			if key != "" && resp.StatusCode == http.StatusOK && body != nil {
				g.cache.put(key, cached{body: body, contentType: resp.Header.Get("Content-Type"), upstream: route.Upstream})
			}
			return
		}
		// Network errors, 429 and 5xx are the upstream's problem: try the next route.
		reason := ""
		if err != nil {
			reason = err.Error()
		} else {
			reason = resp.Status
			_ = resp.Body.Close()
		}
		g.metrics.request(client, alias, route.Upstream, "failover")
		log.Printf("%s %s via %s/%s failed after %s: %s", client, alias, route.Upstream, route.Model, time.Since(start).Round(time.Millisecond), reason)
		failures = append(failures, route.Upstream+": "+reason)
	}
	apiError(w, http.StatusBadGateway, "all upstreams failed: "+strings.Join(failures, "; "))
}

func (g *Gateway) send(r *http.Request, route Route, payload []byte) (*http.Response, error) {
	up := g.cfg.Upstreams[route.Upstream]
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(up.BaseURL, "/")+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if up.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+up.apiKey)
	}
	return g.client.Do(req)
}

type record struct {
	client, alias string
	route         Route
	start         time.Time
}

// relay copies the upstream response to the client and meters its token usage.
// It returns the body of a non-streamed response, so the caller can cache it.
func (g *Gateway) relay(w http.ResponseWriter, resp *http.Response, stream bool, rec record) []byte {
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("X-Gateway-Upstream", rec.route.Upstream)
	w.WriteHeader(resp.StatusCode)

	var u usage
	var body []byte
	if stream && resp.StatusCode == http.StatusOK {
		u = g.pipeStream(w, resp.Body)
	} else {
		var err error
		body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			log.Printf("reading %s response: %v", rec.route.Upstream, err)
		}
		_, _ = w.Write(body)
		var parsed struct {
			Usage *usage `json:"usage"`
		}
		if json.Unmarshal(body, &parsed) == nil && parsed.Usage != nil {
			u = *parsed.Usage
		}
	}

	status := strconv.Itoa(resp.StatusCode)
	g.metrics.request(rec.client, rec.alias, rec.route.Upstream, status)
	cost := g.metrics.usage(rec.client, rec.alias, u, g.cfg.Prices[rec.route.Model])
	log.Printf("%s %s via %s/%s %s in %s, %d+%d tokens, $%.6f", rec.client, rec.alias, rec.route.Upstream,
		rec.route.Model, status, time.Since(rec.start).Round(time.Millisecond), u.Prompt, u.Completion, cost)
	return body
}

type usage struct {
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
}

// pipeStream forwards server-sent events line by line, flushing each event, and picks the
// usage out of the final chunk.
func (g *Gateway) pipeStream(w http.ResponseWriter, body io.Reader) usage {
	flush := http.NewResponseController(w).Flush
	var u usage
	reader := bufio.NewReader(body)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return u // client went away
			}
			if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data: ")); ok && bytes.Contains(data, []byte(`"usage"`)) {
				var chunk struct {
					Usage *usage `json:"usage"`
				}
				if json.Unmarshal(data, &chunk) == nil && chunk.Usage != nil {
					u = *chunk.Usage
				}
			}
			if len(bytes.TrimSpace(line)) == 0 {
				_ = flush()
			}
		}
		if err != nil {
			_ = flush()
			if !errors.Is(err, io.EOF) {
				log.Printf("stream cut: %v", err)
			}
			return u
		}
	}
}

func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.authenticate(r); !ok {
		apiError(w, http.StatusUnauthorized, "invalid or missing API key")
		return
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	list := struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: []model{}}
	for _, a := range g.cfg.aliases() {
		list.Data = append(list.Data, model{ID: a, Object: "model", OwnedBy: "llm-gateway"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

// authenticate returns the client name for the bearer token, or "anonymous" when no keys are configured.
func (g *Gateway) authenticate(r *http.Request) (string, bool) {
	if len(g.cfg.Keys) == 0 {
		return "anonymous", true
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "", false
	}
	for name, k := range g.cfg.Keys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(k.secret)) == 1 {
			return name, true
		}
	}
	return "", false
}

// apiError answers in OpenAI's error shape, so client SDKs surface the message.
func apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "llm_gateway_error", "code": status}})
}

// limiter is GCRA per client: one timestamp each, no refill goroutine.
type limiter struct {
	mu  sync.Mutex
	tat map[string]time.Time
	now func() time.Time
}

func (l *limiter) allow(client string, rpm int) (time.Duration, bool) {
	if rpm <= 0 {
		return 0, true
	}
	interval, burst := time.Minute/time.Duration(rpm), time.Minute
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	tat := l.tat[client]
	if tat.Before(now) {
		tat = now
	}
	next := tat.Add(interval)
	if ahead := next.Sub(now); ahead > burst {
		return ahead - burst, false
	}
	l.tat[client] = next
	return 0, true
}
