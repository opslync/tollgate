package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestLoadBudgets(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
server:
  listen: ":8080"
providers:
  - name: anthropic
    base_url: "https://api.anthropic.com"
agents:
  - name: support-bot
    key: "tg_support_0123456789abcdef"
    team: support
budgets:
  - agent: support-bot
    window: 24h
    limit_usd: 10.0
  - team: support
    window: 7d
    limit_tokens: 1000000
    alert_at: 0.5
    action: throttle
    throttle_interval: 10s
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b0, b1 := cfg.Budgets[0], cfg.Budgets[1]
	if time.Duration(b0.Window) != 24*time.Hour || b0.AlertAt != 0.8 || b0.Action != "block" ||
		time.Duration(b0.ThrottleInterval) != 30*time.Second {
		t.Errorf("budget[0] defaults = %+v", b0)
	}
	if time.Duration(b1.Window) != 7*24*time.Hour || b1.AlertAt != 0.5 || b1.Action != "throttle" ||
		time.Duration(b1.ThrottleInterval) != 10*time.Second {
		t.Errorf("budget[1] = %+v", b1)
	}
}

func TestLoadBudgetErrors(t *testing.T) {
	base := `
server:
  listen: ":8080"
providers:
  - name: a
    base_url: "https://x"
agents:
  - name: bot
    key: "0123456789abcdef"
    team: support
budgets:
`
	tests := []struct {
		name, budget, wantErr string
	}{
		{"both agent and team", "  - agent: bot\n    team: support\n    window: 1h\n    limit_usd: 1\n", "exactly one"},
		{"neither agent nor team", "  - window: 1h\n    limit_usd: 1\n", "exactly one"},
		{"unknown agent", "  - agent: ghost\n    window: 1h\n    limit_usd: 1\n", "unknown agent"},
		{"unknown team", "  - team: ghosts\n    window: 1h\n    limit_usd: 1\n", "no agent belongs"},
		{"missing window", "  - agent: bot\n    limit_usd: 1\n", "window must be set"},
		{"no limits", "  - agent: bot\n    window: 1h\n", "at least one of limit_usd"},
		{"bad alert_at", "  - agent: bot\n    window: 1h\n    limit_usd: 1\n    alert_at: 1.5\n", "alert_at"},
		{"bad action", "  - agent: bot\n    window: 1h\n    limit_usd: 1\n    action: explode\n", "action must be"},
		{"bad window", "  - agent: bot\n    window: fortnight\n    limit_usd: 1\n", "invalid duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, base+tt.budget))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestAdminKeyExpansion(t *testing.T) {
	t.Setenv("TEST_ADMIN_KEY", "super-secret")
	cfg, err := Load(writeConfig(t, `
server:
  listen: ":8080"
  admin_key: "${TEST_ADMIN_KEY}"
providers:
  - name: a
    base_url: "https://x"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AdminKey != "super-secret" {
		t.Errorf("admin_key = %q", cfg.Server.AdminKey)
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
