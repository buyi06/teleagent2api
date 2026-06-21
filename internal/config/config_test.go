package config

import (
	"os"
	"testing"
)

func TestLoadClaudeCodeFriendlyDefaults(t *testing.T) {
	t.Setenv("TELEAGENT_CONFIG", "does-not-exist.json")
	t.Setenv("TELEAGENT_TOKEN", "aaa.bbb.ccc")
	t.Setenv("TELEAGENT_DEVICE_ID", "device")
	t.Setenv("TELEAGENT_INSTALL_ID", "install")

	cfg := Load()
	if cfg.ExposeReasoning {
		t.Fatal("reasoning should be hidden by default for OpenAI-compatible coding clients")
	}
	if cfg.ReasoningToContent {
		t.Fatal("reasoning-to-content fallback should be disabled by default")
	}
	if cfg.MinOutputTokens != 1024 {
		t.Fatalf("unexpected default MinOutputTokens: %d", cfg.MinOutputTokens)
	}
}

func TestLoadReasoningCompatibilityOverrides(t *testing.T) {
	t.Setenv("TELEAGENT_CONFIG", "does-not-exist.json")
	t.Setenv("TELEAGENT_TOKEN", "aaa.bbb.ccc")
	t.Setenv("TELEAGENT_DEVICE_ID", "device")
	t.Setenv("TELEAGENT_INSTALL_ID", "install")
	t.Setenv("TELEAGENT_EXPOSE_REASONING", "true")
	t.Setenv("TELEAGENT_REASONING_TO_CONTENT", "1")
	t.Setenv("TELEAGENT_MIN_OUTPUT_TOKENS", "2048")

	cfg := Load()
	if !cfg.ExposeReasoning {
		t.Fatal("TELEAGENT_EXPOSE_REASONING=true was not applied")
	}
	if !cfg.ReasoningToContent {
		t.Fatal("TELEAGENT_REASONING_TO_CONTENT=1 was not applied")
	}
	if cfg.MinOutputTokens != 2048 {
		t.Fatalf("TELEAGENT_MIN_OUTPUT_TOKENS was not applied: %d", cfg.MinOutputTokens)
	}
}

func TestLoadKeepsMinOutputDefaultWhenConfigFileOmitsIt(t *testing.T) {
	configPath := t.TempDir() + "/config.json"
	if err := os.WriteFile(configPath, []byte(`{
		"token": "aaa.bbb.ccc",
		"deviceId": "device",
		"installId": "install"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELEAGENT_CONFIG", configPath)

	cfg := Load()
	if cfg.MinOutputTokens != 1024 {
		t.Fatalf("missing config minOutputTokens should keep default 1024, got %d", cfg.MinOutputTokens)
	}
}
