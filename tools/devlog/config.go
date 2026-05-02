package main

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Model ModelConfig `toml:"model"`
}

type ModelConfig struct {
	Provider string       `toml:"provider"`
	Model    string       `toml:"model"`
	Backup   *BackupModel `toml:"backup"`
}

type BackupModel struct {
	Provider  string `toml:"provider"`
	Model     string `toml:"model"`
	APIKeyEnv string `toml:"api_key_env"`
}

func defaultConfig() Config {
	return Config{
		Model: ModelConfig{
			Provider: "anthropic",
			Model:    "claude-sonnet-4-6",
		},
	}
}

func loadConfig() Config {
	cfg := defaultConfig()

	configDir, err := os.UserConfigDir()
	if err != nil {
		return cfg
	}

	path := filepath.Join(configDir, "devlog", "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}

	if err := toml.Unmarshal(data, &cfg); err != nil {
		return defaultConfig()
	}

	if cfg.Model.Provider == "" {
		cfg.Model.Provider = "anthropic"
	}
	if cfg.Model.Model == "" {
		cfg.Model.Model = "claude-sonnet-4-6"
	}

	return cfg
}
