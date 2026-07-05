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

func TestLoadAgentsAndProviderKey(t *testing.T) {
	t.Setenv("TEST_UPSTREAM_KEY", "sk-ant-real-key")
	cfg, err := Load(writeConfig(t, `
server:
  listen: ":8080"
providers:
  - name: anthropic
    base_url: "https://api.anthropic.com"
    api_key: "${TEST_UPSTREAM_KEY}"
agents:
  - name: support-bot
    key: "tg_support_0123456789abcdef"
    team: support
    namespace: prod
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers[0].APIKey; got != "sk-ant-real-key" {
		t.Errorf("api_key = %q, want expanded env value", got)
	}
	a := cfg.Agents[0]
	if a.Name != "support-bot" || a.Team != "support" || a.Namespace != "prod" {
		t.Errorf("agent = %+v", a)
	}
}

func TestLoadUnsetEnvKey(t *testing.T) {
	_, err := Load(writeConfig(t, `
server:
  listen: ":8080"
providers:
  - name: anthropic
    base_url: "https://api.anthropic.com"
    api_key: "${TOLLGATE_TEST_DEFINITELY_UNSET}"
`))
	if err == nil || !strings.Contains(err.Error(), "unset environment variable") {
		t.Fatalf("err = %v, want unset env var error", err)
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
		{
			"agent without name",
			"server:\n  listen: \":8080\"\nproviders:\n  - name: a\n    base_url: \"https://x\"\nagents:\n  - key: \"0123456789abcdef\"\n",
			"name must be set",
		},
		{
			"agent key too short",
			"server:\n  listen: \":8080\"\nproviders:\n  - name: a\n    base_url: \"https://x\"\nagents:\n  - name: bot\n    key: \"short\"\n",
			"at least 16 characters",
		},
		{
			"duplicate agent key",
			"server:\n  listen: \":8080\"\nproviders:\n  - name: a\n    base_url: \"https://x\"\nagents:\n  - name: bot1\n    key: \"0123456789abcdef\"\n  - name: bot2\n    key: \"0123456789abcdef\"\n",
			"key already used",
		},
		{
			"duplicate agent name",
			"server:\n  listen: \":8080\"\nproviders:\n  - name: a\n    base_url: \"https://x\"\nagents:\n  - name: bot\n    key: \"0123456789abcdef\"\n  - name: bot\n    key: \"fedcba9876543210\"\n",
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
