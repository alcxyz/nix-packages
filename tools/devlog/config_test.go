package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigPrefersSharedLLMConfig(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	mustWriteFile(t, filepath.Join(cfgDir, "llm", "config.toml"), `
[roles.strong]
provider = "openai"
model = "gpt-5.4"
transport = "cli"
api_key_env = "OPENAI_API_KEY"
`)
	mustWriteFile(t, filepath.Join(cfgDir, "devlog", "config.toml"), `
[model]
provider = "anthropic"
model = "claude-sonnet-4-6"
`)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.Model.Provider != "openai" || cfg.Model.Model != "gpt-5.4" || cfg.Model.Transport != "cli" {
		t.Fatalf("loadConfig() = %+v, want shared OpenAI config", cfg.Model)
	}
}

func TestLoadConfigFallsBackToToolConfig(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	mustWriteFile(t, filepath.Join(cfgDir, "devlog", "config.toml"), `
[model]
provider = "openai"
model = "gpt-5.4-mini"
api_key_env = "OPENAI_API_KEY"
`)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.Model.Provider != "openai" || cfg.Model.Model != "gpt-5.4-mini" {
		t.Fatalf("loadConfig() = %+v, want local OpenAI config", cfg.Model)
	}
	if cfg.Model.Transport != "prefer-api" {
		t.Fatalf("transport = %q, want prefer-api", cfg.Model.Transport)
	}
}

func TestLoadConfigErrorsOnInvalidSharedConfig(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	mustWriteFile(t, filepath.Join(cfgDir, "llm", "config.toml"), `
[roles.strong]
provider = "openai"
model = "gpt-5.4"
`)

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig() error = nil, want invalid shared config error")
	}
}

func TestCallWithTransport(t *testing.T) {
	tests := []struct {
		name         string
		transport    string
		apiAvailable bool
		want         string
		wantErr      bool
	}{
		{"cli", "cli", false, "cli", false},
		{"api", "api", true, "api", false},
		{"api-missing-key", "api", false, "", true},
		{"prefer-cli", "prefer-cli", true, "cli", false},
		{"prefer-api-with-key", "prefer-api", true, "api", false},
		{"prefer-api-no-key", "prefer-api", false, "cli", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := callWithTransport(tt.transport, tt.apiAvailable,
				func() ([]byte, error) { return []byte("cli"), nil },
				func() ([]byte, error) { return []byte("api"), nil },
			)
			if (err != nil) != tt.wantErr {
				t.Fatalf("callWithTransport() err = %v, wantErr %v", err, tt.wantErr)
			}
			if string(got) != tt.want {
				t.Fatalf("callWithTransport() = %q, want %q", got, tt.want)
			}
		})
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
