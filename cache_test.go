package main

import (
	"strings"
	"testing"
	"time"
)

func TestCachesDeterministicRequestsPerClient(t *testing.T) {
	primary := newUpstream(t, completion)
	srv := gateway(t, primary, newUpstream(t, completion), 0, &CacheConfig{TTL: "10m", MaxEntries: 100})

	first, body1 := post(t, srv, "gw-secret", hello)
	second, body2 := post(t, srv, "gw-secret", hello)
	if first.Header.Get("X-Gateway-Cache") != "miss" || second.Header.Get("X-Gateway-Cache") != "hit" || body1 != body2 {
		t.Fatalf("cache headers %q then %q", first.Header.Get("X-Gateway-Cache"), second.Header.Get("X-Gateway-Cache"))
	}
	if second.Header.Get("X-Gateway-Upstream") != "primary" || primary.calls() != 1 {
		t.Fatalf("upstream called %d times", primary.calls())
	}
	// Same request, field order shuffled: still a hit.
	reordered := `{"tools":[{"type":"function"}],"temperature":0,"messages":[{"content":"hi","role":"user"}],"model":"smart"}`
	if resp, _ := post(t, srv, "gw-secret", reordered); resp.Header.Get("X-Gateway-Cache") != "hit" {
		t.Error("JSON field order changed the cache key")
	}
	// Another client never gets team-a's cached answer.
	if resp, _ := post(t, srv, "gw-other", hello); resp.Header.Get("X-Gateway-Cache") != "miss" || primary.calls() != 2 {
		t.Errorf("team-b: %q after %d calls", resp.Header.Get("X-Gateway-Cache"), primary.calls())
	}
	m := scrape(t, srv)
	if !strings.Contains(m, `status="cache_hit"} 2`) || !strings.Contains(m, `llm_gateway_tokens_total{client="team-a",model="smart",kind="prompt"} 1000`) {
		t.Errorf("cache hits must be counted and cost no tokens:\n%s", m)
	}
}

func TestOnlyDeterministicSuccessesAreCached(t *testing.T) {
	primary := newUpstream(t, completion)
	srv := gateway(t, primary, newUpstream(t, completion), 0, &CacheConfig{TTL: "10m", MaxEntries: 100})
	for _, body := range []string{
		`{"model":"smart","messages":[],"temperature":0.7}`,
		`{"model":"smart","messages":[]}`,
		`{"model":"smart","messages":[],"temperature":0,"stream":true}`,
	} {
		post(t, srv, "gw-secret", body)
		if resp, _ := post(t, srv, "gw-secret", body); resp.Header.Get("X-Gateway-Cache") == "hit" {
			t.Errorf("cached a non-deterministic request: %s", body)
		}
	}

	failing := newUpstream(t, status(400))
	srv = gateway(t, failing, newUpstream(t, completion), 0, &CacheConfig{TTL: "10m", MaxEntries: 100})
	post(t, srv, "gw-secret", hello)
	if resp, _ := post(t, srv, "gw-secret", hello); resp.StatusCode != 400 || failing.calls() != 2 {
		t.Errorf("an error response was cached: %d after %d calls", resp.StatusCode, failing.calls())
	}
}

func TestCacheExpiresAndEvicts(t *testing.T) {
	c, err := newCache(&CacheConfig{TTL: "1m", MaxEntries: 2})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }
	c.put("a", cached{body: []byte("A")})
	now = now.Add(61 * time.Second)
	if _, ok := c.get("a"); ok {
		t.Error("entry outlived its ttl")
	}
	for _, k := range []string{"b", "c", "d"} {
		c.put(k, cached{body: []byte(k)})
	}
	if _, ok := c.get("b"); ok {
		t.Error("oldest entry not evicted at max_entries")
	}
	if _, ok := c.get("d"); !ok {
		t.Error("newest entry missing")
	}
	if _, err := newCache(&CacheConfig{TTL: "0s"}); err == nil {
		t.Error("zero ttl accepted")
	}
}
