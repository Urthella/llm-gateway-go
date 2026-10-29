package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstream is a fake OpenAI-compatible provider that records what it was sent.
type upstream struct {
	*httptest.Server
	mu   sync.Mutex
	got  []map[string]any
	auth []string
}

func newUpstream(t *testing.T, handle func(w http.ResponseWriter, req map[string]any)) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		u.mu.Lock()
		u.got = append(u.got, req)
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.mu.Unlock()
		handle(w, req)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) calls() int { u.mu.Lock(); defer u.mu.Unlock(); return len(u.got) }

func completion(w http.ResponseWriter, req map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":"c1","model":%q,"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1000,"completion_tokens":200}}`, req["model"])
}

func status(code int) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) { http.Error(w, `{"error":{"message":"nope"}}`, code) }
}

// gateway wires a primary and a backup upstream behind the alias "smart", with one client key.
// A second client, "team-b" with key "gw-other", exists to check per-client isolation.
func gateway(t *testing.T, primary, backup *upstream, rpm int, cache ...*CacheConfig) *httptest.Server {
	cfg := &Config{
		Upstreams: map[string]Upstream{
			"primary": {BaseURL: primary.URL + "/v1", APIKeyEnv: "PRIMARY_KEY"},
			"backup":  {BaseURL: backup.URL + "/v1"},
		},
		Models: map[string][]Route{"smart": {{"primary", "big-model"}, {"backup", "small-model"}}},
		Prices: map[string]Price{"big-model": {Input: 3, Output: 15}, "small-model": {Input: 0.5, Output: 1}},
		Keys:   map[string]Key{"team-a": {KeyEnv: "TEAM_A", RPM: rpm}, "team-b": {KeyEnv: "TEAM_B"}},
	}
	if len(cache) > 0 {
		cfg.Cache = cache[0]
	}
	env := map[string]string{"PRIMARY_KEY": "sk-upstream", "TEAM_A": "gw-secret", "TEAM_B": "gw-other"}
	if err := cfg.resolve(func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	gw, err := NewGateway(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gw.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, srv *httptest.Server, key, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

const hello = `{"model":"smart","messages":[{"role":"user","content":"hi"}],"temperature":0,"tools":[{"type":"function"}]}`

func TestRoutesAliasAndKeepsUnknownFields(t *testing.T) {
	primary, backup := newUpstream(t, completion), newUpstream(t, completion)
	resp, body := post(t, gateway(t, primary, backup, 0), "gw-secret", hello)
	if resp.StatusCode != 200 || resp.Header.Get("X-Gateway-Upstream") != "primary" || !strings.Contains(body, `"big-model"`) {
		t.Fatalf("%d %s %s", resp.StatusCode, resp.Header.Get("X-Gateway-Upstream"), body)
	}
	got := primary.got[0]
	if got["model"] != "big-model" || got["tools"] == nil || primary.auth[0] != "Bearer sk-upstream" {
		t.Errorf("upstream got %v with auth %q", got, primary.auth[0])
	}
	if backup.calls() != 0 {
		t.Error("backup called although primary succeeded")
	}
}

func TestFallsBackOn5xxAnd429(t *testing.T) {
	for _, code := range []int{500, 503, 429} {
		primary, backup := newUpstream(t, status(code)), newUpstream(t, completion)
		srv := gateway(t, primary, backup, 0)
		resp, body := post(t, srv, "gw-secret", hello)
		if resp.StatusCode != 200 || resp.Header.Get("X-Gateway-Upstream") != "backup" || backup.got[0]["model"] != "small-model" {
			t.Fatalf("primary %d: got %d via %s: %s", code, resp.StatusCode, resp.Header.Get("X-Gateway-Upstream"), body)
		}
		if backup.auth[0] != "" {
			t.Errorf("backup has no key configured but got %q", backup.auth[0])
		}
	}
}

func TestDoesNotFallBackOnClientErrors(t *testing.T) {
	primary, backup := newUpstream(t, status(400)), newUpstream(t, completion)
	resp, _ := post(t, gateway(t, primary, backup, 0), "gw-secret", hello)
	if resp.StatusCode != 400 || backup.calls() != 0 {
		t.Fatalf("a 400 is the request's fault: got %d, backup calls %d", resp.StatusCode, backup.calls())
	}
}

func TestAllUpstreamsDown(t *testing.T) {
	primary, backup := newUpstream(t, status(500)), newUpstream(t, status(502))
	resp, body := post(t, gateway(t, primary, backup, 0), "gw-secret", hello)
	if resp.StatusCode != 502 || !strings.Contains(body, "primary: 500") || !strings.Contains(body, "backup: 502") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestAuthAndRateLimit(t *testing.T) {
	srv := gateway(t, newUpstream(t, completion), newUpstream(t, completion), 2)
	if resp, body := post(t, srv, "", hello); resp.StatusCode != 401 || !strings.Contains(body, `"message":"invalid or missing API key"`) {
		t.Fatalf("no key: %d %s", resp.StatusCode, body)
	}
	if resp, _ := post(t, srv, "wrong", hello); resp.StatusCode != 401 {
		t.Fatalf("wrong key: %d", resp.StatusCode)
	}
	for i := 0; i < 2; i++ {
		if resp, _ := post(t, srv, "gw-secret", hello); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i+1, resp.StatusCode)
		}
	}
	resp, _ := post(t, srv, "gw-secret", hello)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("third request in a 2 rpm burst: %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestLimiterRefills(t *testing.T) {
	now := time.Unix(0, 0)
	l := &limiter{tat: map[string]time.Time{}, now: func() time.Time { return now }}
	for i := 0; i < 60; i++ {
		if _, ok := l.allow("a", 60); !ok {
			t.Fatalf("burst request %d denied", i+1)
		}
	}
	if wait, ok := l.allow("a", 60); ok || wait != time.Second {
		t.Fatalf("61st: ok=%v wait=%s, want a 1s wait", ok, wait)
	}
	now = now.Add(time.Second)
	if _, ok := l.allow("a", 60); !ok {
		t.Fatal("one interval later a request should fit")
	}
	if _, ok := l.allow("b", 60); !ok {
		t.Fatal("clients are limited separately")
	}
}

func TestStreamsEventsAndMetersUsage(t *testing.T) {
	stream := func(w http.ResponseWriter, req map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, part := range []string{"Hel", "lo"} {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", part)
			w.(http.Flusher).Flush()
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}
	primary := newUpstream(t, stream)
	srv := gateway(t, primary, newUpstream(t, completion), 0)
	resp, body := post(t, srv, "gw-secret", `{"model":"smart","stream":true,"messages":[]}`)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, `"content":"lo"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream not passed through:\n%s", body)
	}
	if opts, _ := primary.got[0]["stream_options"].(map[string]any); opts["include_usage"] != true {
		t.Errorf("gateway didn't ask for usage: %v", primary.got[0])
	}
	if m := scrape(t, srv); !strings.Contains(m, `llm_gateway_tokens_total{client="team-a",model="smart",kind="completion"} 2`) {
		t.Errorf("streamed usage not metered:\n%s", m)
	}
}

func TestMetricsAndModels(t *testing.T) {
	srv := gateway(t, newUpstream(t, status(500)), newUpstream(t, completion), 0)
	post(t, srv, "gw-secret", hello)
	m := scrape(t, srv)
	for _, want := range []string{
		`llm_gateway_requests_total{client="team-a",model="smart",upstream="primary",status="failover"} 1`,
		`llm_gateway_requests_total{client="team-a",model="smart",upstream="backup",status="200"} 1`,
		`llm_gateway_tokens_total{client="team-a",model="smart",kind="prompt"} 1000`,
		// backup prices: 1000 x $0.5/M + 200 x $1/M
		`llm_gateway_cost_usd_total{client="team-a",model="smart"} 0.0007`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("missing %s in:\n%s", want, m)
		}
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer gw-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"id":"smart"`) {
		t.Errorf("models: %s", b)
	}
	if resp, body := post(t, srv, "gw-secret", `{"model":"gpt-nope"}`); resp.StatusCode != 404 || !strings.Contains(body, "available: smart") {
		t.Errorf("unknown model: %d %s", resp.StatusCode, body)
	}
}

func TestConfigNeedsSecretsAndValidRoutes(t *testing.T) {
	cfg := &Config{Upstreams: map[string]Upstream{"a": {BaseURL: "http://x", APIKeyEnv: "MISSING"}}, Models: map[string][]Route{"m": {{"a", "x"}}}}
	if err := cfg.resolve(func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "MISSING is not set") {
		t.Errorf("missing upstream key: %v", err)
	}
	cfg = &Config{Upstreams: map[string]Upstream{"a": {BaseURL: "http://x"}}, Models: map[string][]Route{"m": {{"b", "x"}}}}
	if err := cfg.resolve(func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), `unknown upstream "b"`) {
		t.Errorf("bad route: %v", err)
	}
}

func scrape(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
