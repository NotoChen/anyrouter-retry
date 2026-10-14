package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const pluginID = "anyrouter-retry"

type Config struct {
	Enabled        bool     `yaml:"enabled"`
	AcceptNew      bool     `yaml:"accept_new_requests"`
	ConfigPath     string   `yaml:"config_path"`
	Hosts          []string `yaml:"upstream_hosts"`
	Models         []string `yaml:"models"`
	MaxAttempts    int      `yaml:"max_attempts"`
	Heartbeat      string   `yaml:"heartbeat_interval"`
	RetryDelay     string   `yaml:"retry_delay"`
	MaxBuffer      int      `yaml:"max_bootstrap_bytes"`
	MaxFrame       int      `yaml:"max_sse_frame_bytes"`
	TransientCodes []string `yaml:"transient_codes"`
	TraceDir       string   `yaml:"trace_dir"`
	TraceRetention int      `yaml:"trace_retention_days"`
	TraceMaxBytes  int64    `yaml:"trace_max_bytes_per_request"`
	heartbeat      time.Duration
	delay          time.Duration
}

var currentConfig atomic.Pointer[Config]

func defaultConfig() Config {
	return Config{Enabled: true, AcceptNew: true, ConfigPath: "/CLIProxyAPI/config.yaml", Hosts: []string{"anyrouter.top"}, Models: []string{"gpt-6-astra"}, Heartbeat: "5s", RetryDelay: "0s", MaxBuffer: 8 << 20, MaxFrame: 64 << 20, TraceDir: "/CLIProxyAPI/plugins/anyrouter-retry-data", TraceRetention: 7, TraceMaxBytes: 32 << 20,
		TransientCodes: []string{"rate_limit_reached", "rate_limit_exceeded", "rate_limit_error", "too_many_requests", "no_capacity", "get_channel_failed", "server_error", "overloaded_error", "service_unavailable"}}
}

func parseConfig(raw []byte) (*Config, error) {
	c := defaultConfig()
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("invalid plugin config")
		}
	}
	var err error
	if c.heartbeat, err = time.ParseDuration(c.Heartbeat); err != nil || c.heartbeat < 100*time.Millisecond {
		return nil, fmt.Errorf("heartbeat_interval must be at least 100ms")
	}
	if c.delay, err = time.ParseDuration(c.RetryDelay); err != nil || c.delay < 0 {
		return nil, fmt.Errorf("retry_delay must be non-negative")
	}
	if c.MaxAttempts < 0 || c.MaxBuffer < 1024 || c.MaxFrame < 1024 || c.ConfigPath == "" || len(c.Hosts) == 0 || len(c.Models) == 0 || c.TraceDir == "" || c.TraceRetention < 1 || c.TraceMaxBytes < 4096 {
		return nil, fmt.Errorf("invalid retry limits or routing configuration")
	}
	return &c, nil
}

type credential struct {
	Key     string            `yaml:"api-key"`
	BaseURL string            `yaml:"base-url"`
	Proxy   string            `yaml:"proxy-url"`
	Prefix  string            `yaml:"prefix"`
	Headers map[string]string `yaml:"headers"`
	Models  []struct {
		Name  string `yaml:"name"`
		Alias string `yaml:"alias"`
	} `yaml:"models"`
	Excluded    []string `yaml:"excluded-models"`
	Retry       *int     `yaml:"request-retry"`
	Disabled    bool     `yaml:"disabled"`
	DisplayName string   `yaml:"display-name"`
	ID          string
	Index       string
}

// Resolve IDs using the pinned CPA v8 synthesizer contract. Secrets are read
// from the existing host config, used only for hashing, and never duplicated.
func loadCredentials(c *Config) ([]credential, error) {
	raw, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read CPA config")
	}
	var doc struct {
		Legacy  []credential `yaml:"codex-api-key"`
		APIKeys yaml.Node    `yaml:"api-keys"`
	}
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("cannot parse CPA config")
	}
	entries := doc.Legacy
	if doc.APIKeys.Kind == yaml.MappingNode {
		var grouped struct {
			Codex []yaml.Node `yaml:"codex"`
		}
		if err = doc.APIKeys.Decode(&grouped); err != nil {
			return nil, fmt.Errorf("cannot parse grouped Codex config")
		}
		entries = nil
		for _, node := range grouped.Codex {
			var group credential
			var children struct {
				Keys []yaml.Node `yaml:"keys"`
			}
			if node.Decode(&group) != nil || node.Decode(&children) != nil {
				return nil, fmt.Errorf("invalid Codex group")
			}
			if len(children.Keys) == 0 {
				entries = append(entries, group)
				continue
			}
			for _, child := range children.Keys {
				inherited := group
				if child.Decode(&inherited) != nil {
					return nil, fmt.Errorf("invalid Codex key")
				}
				entries = append(entries, inherited)
			}
		}
	}
	counters := map[string]int{}
	for i := range entries {
		e := &entries[i]
		h := sha256.New()
		h.Write([]byte("codex:apikey"))
		keys := make([]string, 0, len(e.Headers))
		for k := range e.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var hdr strings.Builder
		for _, k := range keys {
			hdr.WriteString(k)
			hdr.WriteByte(0)
			hdr.WriteString(e.Headers[k])
			hdr.WriteByte(0)
		}
		for _, part := range []string{e.Key, e.BaseURL, e.Proxy, e.Prefix, hdr.String()} {
			h.Write([]byte{0})
			h.Write([]byte(strings.TrimSpace(part)))
		}
		id := "codex:apikey:" + hex.EncodeToString(h.Sum(nil))[:12]
		if n := counters[id]; n > 0 {
			e.ID = fmt.Sprintf("%s-%d", id, n)
		} else {
			e.ID = id
		}
		counters[id]++
		indexHash := sha256.Sum256([]byte("codex-api-key:" + strings.TrimSpace(e.BaseURL) + "+" + strings.TrimSpace(e.Key)))
		e.Index = hex.EncodeToString(indexHash[:8])
		e.Key = "" // Do not retain keys beyond credential discovery.
	}
	return entries, nil
}

func baseModel(model string) string {
	if i := strings.LastIndex(model, "("); i >= 0 && strings.HasSuffix(model, ")") {
		return model[:i]
	}
	return model
}

func (e credential) supports(model string) bool {
	model = baseModel(model)
	for _, x := range e.Excluded {
		if ok, _ := path.Match(x, model); ok {
			return false
		}
	}
	if e.Disabled {
		return false
	}
	if e.Prefix != "" {
		p := strings.Trim(e.Prefix, "/") + "/"
		if !strings.HasPrefix(model, p) {
			return false
		}
		model = strings.TrimPrefix(model, p)
	}
	for _, m := range e.Models {
		name := m.Alias
		if name == "" {
			name = m.Name
		}
		if name == model {
			return true
		}
	}
	return false
}

func (c *Config) isAny(e credential) bool {
	u, err := url.Parse(e.BaseURL)
	if err != nil {
		return false
	}
	for _, host := range c.Hosts {
		if strings.EqualFold(u.Hostname(), host) {
			return true
		}
	}
	return false
}

// Router executes before provider selection. Only take exclusive AnyRouter
// models, so adding the same model to another supplier cannot hijack its route.
func candidates(c *Config, model string, strict bool) ([]credential, error) {
	allowed := false
	for _, m := range c.Models {
		if baseModel(model) == m {
			allowed = true
		}
	}
	if !allowed {
		return nil, nil
	}
	entries, err := loadCredentials(c)
	if err != nil {
		return nil, err
	}
	var out []credential
	for _, e := range entries {
		if !e.supports(model) {
			continue
		}
		if !c.isAny(e) {
			return nil, nil
		}
		if strict && (e.Retry == nil || *e.Retry != 0) {
			return nil, fmt.Errorf("AnyRouter credentials must set request-retry: 0 to avoid nested retries")
		}
		out = append(out, e)
	}
	return out, nil
}
