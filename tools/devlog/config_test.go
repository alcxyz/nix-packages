package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigReadsStandardRole(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	mustWriteFile(t, filepath.Join(cfgDir, "llm", "config.toml"), `
[roles.standard]
provider = "openai"
model = "gpt-5.4"
transport = "cli"
api_key_env = "OPENAI_API_KEY"

[roles.light]
provider = "anthropic"
model = "haiku"
transport = "cli"
`)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.Model.Provider != "openai" || cfg.Model.Model != "gpt-5.4" || cfg.Model.Transport != "cli" {
		t.Fatalf("loadConfig() = %+v, want shared standard role", cfg.Model)
	}
}

func TestLoadConfigIgnoresToolConfig(t *testing.T) {
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
	want := defaultConfig().Model
	if cfg.Model.Provider != want.Provider || cfg.Model.Model != want.Model || cfg.Model.Transport != want.Transport || cfg.Model.Backup != nil {
		t.Fatalf("loadConfig() = %+v, want built-in default %+v", cfg.Model, want)
	}
	if want.Provider != "anthropic" || want.Model != "opus" || want.Transport != "cli" {
		t.Fatalf("defaultConfig() = %+v, want anthropic opus cli", want)
	}
}

func TestLoadConfigErrorsOnMissingStandardRole(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	mustWriteFile(t, filepath.Join(cfgDir, "llm", "config.toml"), `
[roles.strong]
provider = "openai"
model = "gpt-5.4"
transport = "cli"
`)

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig() error = nil, want missing roles.standard error")
	}
}

func TestLoadConfigErrorsOnInvalidSharedConfig(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	mustWriteFile(t, filepath.Join(cfgDir, "llm", "config.toml"), `
[roles.standard]
provider = "openai"
model = "gpt-5.4"
`)

	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig() error = nil, want invalid shared config error")
	}
}

func TestLoadConfigErrorsOnMissingModel(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"role", `
[roles.standard]
provider = "anthropic"
transport = "api"
`},
		{"backup", `
[roles.standard]
provider = "openai"
model = "gpt-5.4"
transport = "cli"

[roles.standard.backup]
provider = "anthropic"
transport = "cli"
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgDir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", cfgDir)
			path := filepath.Join(cfgDir, "llm", "config.toml")
			mustWriteFile(t, path, tt.config)

			_, err := loadConfig()
			if err == nil {
				t.Fatal("loadConfig() error = nil, want missing model error")
			}
			msg := err.Error()
			if !strings.Contains(msg, "missing model") || !strings.Contains(msg, "role standard") || !strings.Contains(msg, path) {
				t.Fatalf("loadConfig() error = %q, want missing model naming role standard and %s", msg, path)
			}
		})
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

func TestLoadConfigReadsEffort(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	mustWriteFile(t, filepath.Join(cfgDir, "llm", "config.toml"), `
[roles.standard]
provider = "openai"
model = "gpt-5.4"
transport = "cli"
effort = "medium"

[roles.standard.backup]
provider = "anthropic"
model = "opus"
transport = "cli"
effort = "high"
`)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.Model.Effort != "medium" || cfg.Model.Backup == nil || cfg.Model.Backup.Effort != "high" {
		t.Fatalf("loadConfig() = %+v, want efforts medium and high", cfg.Model)
	}
}
