package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Config is loaded from a JSON file. Secrets never live in it: upstream API keys and gateway
// client keys are read from the environment variables it names.
type Config struct {
	Listen    string              `json:"listen"`
	Upstreams map[string]Upstream `json:"upstreams"`
	Models    map[string][]Route  `json:"models"` // alias -> routes tried in order
	Prices    map[string]Price    `json:"prices"` // upstream model -> USD per million tokens
	Keys      map[string]Key      `json:"keys"`   // client name -> key; empty means no auth (local use)
}

// Upstream is any OpenAI-compatible API: OpenAI, Anthropic's compatibility endpoint, Ollama, vLLM, ...
type Upstream struct {
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
	apiKey    string
}

type Route struct {
	Upstream string `json:"upstream"`
	Model    string `json:"model"`
}

type Price struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
}

type Key struct {
	KeyEnv string `json:"key_env"`
	RPM    int    `json:"rpm"` // requests per minute, bursting up to a minute's worth; 0 = unlimited
	secret string
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, cfg.resolve(os.Getenv)
}

// resolve reads secrets from the environment and checks that every route points somewhere real.
func (c *Config) resolve(getenv func(string) string) error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	for name, up := range c.Upstreams {
		if up.BaseURL == "" {
			return fmt.Errorf("upstream %q has no base_url", name)
		}
		if up.APIKeyEnv != "" {
			if up.apiKey = getenv(up.APIKeyEnv); up.apiKey == "" {
				return fmt.Errorf("upstream %q: %s is not set", name, up.APIKeyEnv)
			}
		}
		c.Upstreams[name] = up
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("no models configured")
	}
	for alias, routes := range c.Models {
		if len(routes) == 0 {
			return fmt.Errorf("model %q has no routes", alias)
		}
		for _, r := range routes {
			if _, ok := c.Upstreams[r.Upstream]; !ok || r.Model == "" {
				return fmt.Errorf("model %q routes to unknown upstream %q or has no model", alias, r.Upstream)
			}
		}
	}
	for name, k := range c.Keys {
		if k.secret = getenv(k.KeyEnv); k.secret == "" {
			return fmt.Errorf("key %q: %s is not set", name, k.KeyEnv)
		}
		c.Keys[name] = k
	}
	return nil
}

func (c *Config) aliases() []string {
	out := make([]string, 0, len(c.Models))
	for a := range c.Models {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
