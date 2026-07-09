// Package config loads and validates Tollgate's YAML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server     Server     `yaml:"server"`
	Storage    Storage    `yaml:"storage"`
	Providers  []Provider `yaml:"providers"`
	Agents     []Agent    `yaml:"agents"`
	Budgets    []Budget   `yaml:"budgets"`
	Kubernetes Kubernetes `yaml:"kubernetes"`
	Teams      []Team     `yaml:"teams"`
	Tracing    Tracing    `yaml:"tracing"`
}

// Tracing enables OTLP/HTTP JSON trace export: one span per proxied request,
// POSTed to a collector. Off by default so non-observability installs stay
// zero-dependency. TLS vs plaintext is determined by the endpoint URL scheme.
type Tracing struct {
	Enabled bool `yaml:"enabled"`
	// OTLPEndpoint is the full traces URL incl. path, e.g.
	// http://otel-collector:4318/v1/traces.
	OTLPEndpoint string `yaml:"otlp_endpoint"`
}

// Kubernetes enables in-cluster identity: agent pods are attributed from their
// ServiceAccount token via TokenReview, with no per-agent Tollgate key. Off by
// default so non-K8s installs stay zero-dependency.
type Kubernetes struct {
	Enabled bool `yaml:"enabled"`
	// Namespaces is reserved for future scoping; an empty list means the pod
	// cache reads across all namespaces (the ClusterRole is cluster-wide either way).
	Namespaces   []string `yaml:"namespaces"`
	PollInterval Duration `yaml:"poll_interval"` // pod-cache refresh; default 15s, must be >= 1s
	// Audiences is the TokenReview audience allowlist; empty accepts any.
	Audiences []string `yaml:"audiences"`
}

// Team maps a set of namespaces to a team name for attribution. A namespace's
// tollgate.io/team label, resolved at runtime, takes precedence over this list.
type Team struct {
	Name       string   `yaml:"name"`
	Namespaces []string `yaml:"namespaces"`
}

type Server struct {
	Listen string `yaml:"listen"`
	// AdminKey, when set, enables the /admin endpoints (kill switch).
	// Supports ${ENV_VAR} references.
	AdminKey string `yaml:"admin_key"`
}

// Budget is a rolling-window spend limit for one agent or one team.
type Budget struct {
	Agent string `yaml:"agent"` // exactly one of Agent or Team
	Team  string `yaml:"team"`
	// Window is a rolling duration: Go syntax plus an integer d suffix (7d).
	Window Duration `yaml:"window"`
	// At least one limit must be set. LimitTokens counts input + output.
	LimitUSD    float64 `yaml:"limit_usd"`
	LimitTokens int64   `yaml:"limit_tokens"`
	// AlertAt is the fraction of the limit that triggers a warning log.
	AlertAt float64 `yaml:"alert_at"` // default 0.8
	// Action at the limit: "block" (403) or "throttle" (429 + Retry-After,
	// one request allowed per ThrottleInterval).
	Action           string   `yaml:"action"`            // default "block"
	ThrottleInterval Duration `yaml:"throttle_interval"` // default 30s
}

// Duration parses Go durations plus an integer day suffix ("7d").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return fmt.Errorf("invalid duration %q", s)
		}
		*d = Duration(time.Duration(n) * 24 * time.Hour)
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(parsed)
	return nil
}

type Storage struct {
	// Path to the SQLite database file; defaults to tollgate.db.
	Path string `yaml:"path"`
}

type Provider struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
	// Type selects the wire protocol: "anthropic" (default) or "openai"
	// (OpenAI-compatible, incl. vLLM). It decides usage parsing, credential
	// header, and which paths route here (/v1/chat/completions etc. go to
	// the openai provider).
	Type string `yaml:"type"`
	// APIKey, when set, replaces the caller's credentials on the upstream
	// request. Supports ${ENV_VAR} references so secrets stay out of YAML.
	APIKey string `yaml:"api_key"`
}

type Agent struct {
	Name      string `yaml:"name"`
	Key       string `yaml:"key"`
	Team      string `yaml:"team"`
	Namespace string `yaml:"namespace"`
}

// minAgentKeyLen guards against trivially guessable agent keys: once a
// provider api_key is configured, agent keys are what stand between the
// internet and your LLM bill.
const minAgentKeyLen = 16

// Load reads the YAML file at path. Unknown fields are rejected so config
// typos fail at startup instead of being silently ignored.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Storage.Path == "" {
		cfg.Storage.Path = "tollgate.db"
	}
	if err := cfg.expandEnv(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	cfg.applyDefaults()
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Server.Listen == "" {
		return fmt.Errorf("server.listen must be set")
	}
	if len(c.Providers) == 0 {
		return fmt.Errorf("at least one provider must be configured")
	}
	seen := make(map[string]bool)
	seenType := make(map[string]string)
	for i, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("providers[%d]: name must be set", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("providers[%d]: duplicate name %q", i, p.Name)
		}
		seen[p.Name] = true
		if p.Type != "" && p.Type != "anthropic" && p.Type != "openai" {
			return fmt.Errorf("providers[%d] (%s): type must be anthropic or openai, got %q", i, p.Name, p.Type)
		}
		typ := p.Type
		if typ == "" {
			typ = "anthropic"
		}
		if other, dup := seenType[typ]; dup {
			return fmt.Errorf("providers[%d] (%s): only one provider per type for now (%s is already %s)", i, p.Name, other, typ)
		}
		seenType[typ] = p.Name
		u, err := url.Parse(p.BaseURL)
		if err != nil {
			return fmt.Errorf("providers[%d] (%s): invalid base_url: %w", i, p.Name, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("providers[%d] (%s): base_url must be http(s)://host[:port], got %q", i, p.Name, p.BaseURL)
		}
	}

	agentNames := make(map[string]bool)
	agentKeys := make(map[string]bool)
	for i, a := range c.Agents {
		if a.Name == "" {
			return fmt.Errorf("agents[%d]: name must be set", i)
		}
		if agentNames[a.Name] {
			return fmt.Errorf("agents[%d]: duplicate name %q", i, a.Name)
		}
		agentNames[a.Name] = true
		if len(a.Key) < minAgentKeyLen {
			return fmt.Errorf("agents[%d] (%s): key must be at least %d characters", i, a.Name, minAgentKeyLen)
		}
		if agentKeys[a.Key] {
			return fmt.Errorf("agents[%d] (%s): key already used by another agent", i, a.Name)
		}
		agentKeys[a.Key] = true
	}

	teams := make(map[string]bool)
	teamNames := make(map[string]bool)
	claimedNS := make(map[string]string)
	for i, t := range c.Teams {
		if t.Name == "" {
			return fmt.Errorf("teams[%d]: name must be set", i)
		}
		if teamNames[t.Name] {
			return fmt.Errorf("teams[%d]: duplicate team name %q", i, t.Name)
		}
		teamNames[t.Name] = true
		teams[t.Name] = true
		for _, ns := range t.Namespaces {
			if other, dup := claimedNS[ns]; dup {
				return fmt.Errorf("teams[%d] (%s): namespace %q already claimed by team %q", i, t.Name, ns, other)
			}
			claimedNS[ns] = t.Name
		}
	}
	// Budget team references may resolve to a teams[] entry or to any team
	// named inline on an agent — teams[] is optional, so inline-only configs
	// keep validating exactly as before.
	for _, a := range c.Agents {
		if a.Team != "" {
			teams[a.Team] = true
		}
	}
	if c.Kubernetes.PollInterval != 0 && time.Duration(c.Kubernetes.PollInterval) < time.Second {
		return fmt.Errorf("kubernetes.poll_interval must be at least 1s")
	}
	for i, b := range c.Budgets {
		if (b.Agent == "") == (b.Team == "") {
			return fmt.Errorf("budgets[%d]: exactly one of agent or team must be set", i)
		}
		if b.Agent != "" && !agentNames[b.Agent] {
			return fmt.Errorf("budgets[%d]: unknown agent %q", i, b.Agent)
		}
		if b.Team != "" && !teams[b.Team] {
			return fmt.Errorf("budgets[%d]: no agent belongs to team %q", i, b.Team)
		}
		if b.Window <= 0 {
			return fmt.Errorf("budgets[%d] (%s): window must be set", i, b.target())
		}
		if b.LimitUSD <= 0 && b.LimitTokens <= 0 {
			return fmt.Errorf("budgets[%d] (%s): at least one of limit_usd or limit_tokens must be positive", i, b.target())
		}
		if b.AlertAt < 0 || b.AlertAt > 1 {
			return fmt.Errorf("budgets[%d] (%s): alert_at must be within (0, 1]", i, b.target())
		}
		if b.Action != "" && b.Action != "block" && b.Action != "throttle" {
			return fmt.Errorf("budgets[%d] (%s): action must be block or throttle, got %q", i, b.target(), b.Action)
		}
	}
	if c.Tracing.Enabled {
		if c.Tracing.OTLPEndpoint == "" {
			return fmt.Errorf("tracing.enabled requires tracing.otlp_endpoint")
		}
		u, err := url.Parse(c.Tracing.OTLPEndpoint)
		if err != nil {
			return fmt.Errorf("tracing.otlp_endpoint invalid: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("tracing.otlp_endpoint must be http(s)://host[:port]/path, got %q", c.Tracing.OTLPEndpoint)
		}
	}
	return nil
}

func (b Budget) target() string {
	if b.Agent != "" {
		return "agent " + b.Agent
	}
	return "team " + b.Team
}

// applyDefaults fills optional fields after validation.
func (c *Config) applyDefaults() {
	for i := range c.Providers {
		if c.Providers[i].Type == "" {
			c.Providers[i].Type = "anthropic"
		}
	}
	for i := range c.Budgets {
		b := &c.Budgets[i]
		if b.AlertAt == 0 {
			b.AlertAt = 0.8
		}
		if b.Action == "" {
			b.Action = "block"
		}
		if b.ThrottleInterval <= 0 {
			b.ThrottleInterval = Duration(30 * time.Second)
		}
	}
	if c.Kubernetes.Enabled && c.Kubernetes.PollInterval == 0 {
		c.Kubernetes.PollInterval = Duration(15 * time.Second)
	}
}

// expandEnv resolves ${VAR} references in secret-bearing fields. A reference
// to an unset or empty variable is an error: silently proxying with an empty
// upstream key would fail in a far more confusing place.
func (c *Config) expandEnv() error {
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.APIKey == "" {
			continue
		}
		expanded, err := expandSecret(p.APIKey)
		if err != nil {
			return fmt.Errorf("providers[%d] (%s): api_key %w", i, p.Name, err)
		}
		p.APIKey = expanded
	}
	if c.Server.AdminKey != "" {
		expanded, err := expandSecret(c.Server.AdminKey)
		if err != nil {
			return fmt.Errorf("server.admin_key %w", err)
		}
		c.Server.AdminKey = expanded
	}
	return nil
}

func expandSecret(s string) (string, error) {
	var missing []string
	expanded := os.Expand(s, func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("references unset environment variable(s): %v", missing)
	}
	return expanded, nil
}
