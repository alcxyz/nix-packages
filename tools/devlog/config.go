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
	Provider  string       `toml:"provider"`
	Model     string       `toml:"model"`
	Transport string       `toml:"transport"`
	APIKeyEnv string       `toml:"api_key_env"`
	Backup    *ModelConfig `toml:"backup"`
}

type sharedConfig struct {
	Roles map[string]ModelConfig `toml:"roles"`
}

func defaultConfig() Config {
	return Config{
		Model: ModelConfig{
			Provider:  "anthropic",
			Model:     "claude-sonnet-4-6",
			Transport: "prefer-cli",
		},
	}
}

func loadConfig() (Config, error) {
	if shared, found, err := loadSharedRoleConfig("strong"); err != nil {
		return Config{}, err
	} else if found {
		return Config{Model: *shared}, nil
	}

	configDir, err := os.UserConfigDir()
	if err != nil {
		return defaultConfig(), nil
	}

	path := filepath.Join(configDir, "devlog", "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultConfig(), nil
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return defaultConfig(), nil
	}
	if err := normalizeModelConfig(&cfg.Model, false); err != nil {
		return defaultConfig(), nil
	}

	return cfg, nil
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
	if err := normalizeModelConfig(&rc, true); err != nil {
		return nil, false, fmt.Errorf("invalid shared llm config %s role %s: %w", path, role, err)
	}

	return &rc, true, nil
}

func normalizeModelConfig(cfg *ModelConfig, requireTransport bool) error {
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.Model == "" {
		switch cfg.Provider {
		case "anthropic":
			cfg.Model = "claude-sonnet-4-6"
		default:
			return fmt.Errorf("missing model")
		}
	}
	if cfg.Transport == "" {
		if requireTransport {
			return fmt.Errorf("missing transport for provider %s", cfg.Provider)
		}
		switch cfg.Provider {
		case "anthropic":
			cfg.Transport = "prefer-cli"
		case "openai":
			cfg.Transport = "prefer-api"
		default:
			return fmt.Errorf("unsupported provider: %s", cfg.Provider)
		}
	}
	switch cfg.Transport {
	case "cli", "api", "prefer-cli", "prefer-api":
	default:
		return fmt.Errorf("unsupported transport %q", cfg.Transport)
	}
	if cfg.Backup != nil {
		if err := normalizeModelConfig(cfg.Backup, requireTransport); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	return nil
}
