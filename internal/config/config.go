// Package config loads and validates Tollgate's YAML configuration.
package config

import (
	"fmt"
	"net/url"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    Server     `yaml:"server"`
	Providers []Provider `yaml:"providers"`
	Agents    []Agent    `yaml:"agents"`
}

type Server struct {
	Listen string `yaml:"listen"`
}

type Provider struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
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
	if err := cfg.expandEnv(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
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
	for i, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("providers[%d]: name must be set", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("providers[%d]: duplicate name %q", i, p.Name)
		}
		seen[p.Name] = true
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
	return nil
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
		var missing []string
		p.APIKey = os.Expand(p.APIKey, func(name string) string {
			v := os.Getenv(name)
			if v == "" {
				missing = append(missing, name)
			}
			return v
		})
		if len(missing) > 0 {
			return fmt.Errorf("providers[%d] (%s): api_key references unset environment variable(s): %v", i, p.Name, missing)
		}
	}
	return nil
}
