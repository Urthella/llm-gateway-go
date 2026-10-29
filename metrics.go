package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// metrics are plain in-memory counters, written in the Prometheus text format by hand.
type metrics struct {
	mu       sync.Mutex
	requests map[[4]string]int64   // client, model, upstream, status
	tokens   map[[3]string]int64   // client, model, kind
	cost     map[[2]string]float64 // client, model
}

func newMetrics() *metrics {
	return &metrics{requests: map[[4]string]int64{}, tokens: map[[3]string]int64{}, cost: map[[2]string]float64{}}
}

func (m *metrics) request(client, model, upstream, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[4]string{client, model, upstream, status}]++
}

// usage adds tokens and their estimated cost, and returns that cost in USD.
func (m *metrics) usage(client, model string, u usage, p Price) float64 {
	cost := (float64(u.Prompt)*p.Input + float64(u.Completion)*p.Output) / 1e6
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[[3]string{client, model, "prompt"}] += u.Prompt
	m.tokens[[3]string{client, model, "completion"}] += u.Completion
	m.cost[[2]string{client, model}] += cost
	return cost
}

func (m *metrics) text() string {
	var w strings.Builder
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprintln(&w, "# HELP llm_gateway_requests_total Requests by client, model alias, upstream and outcome (HTTP status or failover).")
	fmt.Fprintln(&w, "# TYPE llm_gateway_requests_total counter")
	for _, k := range sortedKeys(m.requests) {
		fmt.Fprintf(&w, "llm_gateway_requests_total{client=\"%s\",model=\"%s\",upstream=\"%s\",status=\"%s\"} %d\n", esc(k[0]), esc(k[1]), esc(k[2]), esc(k[3]), m.requests[k])
	}
	fmt.Fprintln(&w, "# HELP llm_gateway_tokens_total Tokens reported by upstreams.")
	fmt.Fprintln(&w, "# TYPE llm_gateway_tokens_total counter")
	for _, k := range sortedKeys(m.tokens) {
		fmt.Fprintf(&w, "llm_gateway_tokens_total{client=\"%s\",model=\"%s\",kind=\"%s\"} %d\n", esc(k[0]), esc(k[1]), esc(k[2]), m.tokens[k])
	}
	fmt.Fprintln(&w, "# HELP llm_gateway_cost_usd_total Estimated cost from the configured prices.")
	fmt.Fprintln(&w, "# TYPE llm_gateway_cost_usd_total counter")
	for _, k := range sortedKeys(m.cost) {
		fmt.Fprintf(&w, "llm_gateway_cost_usd_total{client=\"%s\",model=\"%s\"} %g\n", esc(k[0]), esc(k[1]), m.cost[k])
	}
	return w.String()
}

// esc applies the Prometheus label-value escapes: backslash, double quote and newline, nothing else.
var esc = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace

func sortedKeys[K [2]string | [3]string | [4]string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	return keys
}
