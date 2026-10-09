package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Model ModelConfig `toml:"model"`
}

type ModelConfig struct {
	Provider  string `toml:"provider"`
	Model     string `toml:"model"`
	Transport string `toml:"transport"`
	// Effort is the reasoning effort passed to the CLI transports; empty keeps
	// the CLI's default. The API transports ignore it.
	Effort    string       `toml:"effort"`
	APIKeyEnv string       `toml:"api_key_env"`
	Backup    *ModelConfig `toml:"backup"`
}

type sharedConfig struct {
	Roles map[string]ModelConfig `toml:"roles"`
}

// devlogRole is the agent role devlog requests from the shared LLM config.
const devlogRole = "standard"

// defaultConfig is used when the shared LLM config does not exist. "opus" is
// a Claude Code alias, not an API model ID, so it must use the CLI transport.
func defaultConfig() Config {
	return Config{
		Model: ModelConfig{
			Provider:  "anthropic",
			Model:     "opus",
			Transport: "cli",
		},
	}
}

func loadConfig() (Config, error) {
	shared, found, err := loadSharedRoleConfig(devlogRole)
	if err != nil {
		return Config{}, err
	}
	if !found {
		return defaultConfig(), nil
	}
	return Config{Model: *shared}, nil
}

func loadSharedRoleConfig(role string) (*ModelConfig, bool, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, false, nil
	}

	path := filepath.Join(configDir, "llm", "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read shared llm config: %w", err)
	}

	var cfg sharedConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, false, fmt.Errorf("parse shared llm config %s: %w", path, err)
	}

	rc, ok := cfg.Roles[role]
	if !ok {
		return nil, false, fmt.Errorf("shared llm config %s is missing roles.%s", path, role)
	}
	if err := normalizeModelConfig(&rc); err != nil {
		return nil, false, fmt.Errorf("invalid shared llm config %s role %s: %w", path, role, err)
	}

	return &rc, true, nil
}

func normalizeModelConfig(cfg *ModelConfig) error {
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.Model == "" {
		return fmt.Errorf("missing model for provider %s", cfg.Provider)
	}
	if cfg.Transport == "" {
		return fmt.Errorf("missing transport for provider %s", cfg.Provider)
	}
	switch cfg.Transport {
	case "cli", "api", "prefer-cli", "prefer-api":
	default:
		return fmt.Errorf("unsupported transport %q", cfg.Transport)
	}
	if cfg.Backup != nil {
		if err := normalizeModelConfig(cfg.Backup); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	return nil
}
