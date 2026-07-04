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
}

type Server struct {
	Listen string `yaml:"listen"`
}

type Provider struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
}

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
	return nil
}
