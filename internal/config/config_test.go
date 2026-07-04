package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
server:
  listen: ":8080"
providers:
  - name: anthropic
    base_url: "https://api.anthropic.com"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Errorf("listen = %q, want :8080", cfg.Server.Listen)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "anthropic" {
		t.Errorf("providers = %+v", cfg.Providers)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, yaml, wantErr string
	}{
		{
			"unknown field",
			"server:\n  listen: \":8080\"\n  bogus: true\nproviders:\n  - name: a\n    base_url: \"https://x\"\n",
			"bogus",
		},
		{
			"no providers",
			"server:\n  listen: \":8080\"\n",
			"at least one provider",
		},
		{
			"missing listen",
			"providers:\n  - name: a\n    base_url: \"https://x\"\n",
			"server.listen",
		},
		{
			"bad base_url scheme",
			"server:\n  listen: \":8080\"\nproviders:\n  - name: a\n    base_url: \"ftp://x\"\n",
			"base_url",
		},
		{
			"provider without name",
			"server:\n  listen: \":8080\"\nproviders:\n  - base_url: \"https://x\"\n",
			"name must be set",
		},
		{
			"duplicate provider name",
			"server:\n  listen: \":8080\"\nproviders:\n  - name: a\n    base_url: \"https://x\"\n  - name: a\n    base_url: \"https://y\"\n",
			"duplicate name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.yaml))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}
