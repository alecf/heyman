package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/viper"
)

// Config represents the entire heyman configuration
type Config struct {
	DefaultProfile string             `toml:"default_profile"`
	CacheDays      int                `toml:"cache_days"`
	Profiles       map[string]Profile `toml:"profiles"`
}

// Profile represents an LLM provider configuration
type Profile struct {
	Name          string         `toml:"-"` // Set from map key
	Provider      string         `toml:"provider"` // "openai", "anthropic", "ollama"
	Model         string         `toml:"model"`
	ContextWindow int            `toml:"context_window,omitempty"` // Max context window in tokens (defaults to 8192)
	Options       map[string]any `toml:"options,omitempty"`
}

// Load reads the configuration from the config file and environment variables
func Load() (*Config, error) {
	// Set config defaults
	cfg := &Config{
		CacheDays: 30,
		Profiles:  make(map[string]Profile),
	}

	// Get config file path
	configPath := getConfigPath()

	// Check if config file exists
	if _, err := os.Stat(configPath); err == nil {
		// Config file exists, read it
		data, err := os.ReadFile(configPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}

		if err := toml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config file: %w", err)
		}

		// Set profile names from map keys
		for name, profile := range cfg.Profiles {
			profile.Name = name
			cfg.Profiles[name] = profile
		}
	}

	// Override with environment variables if set
	if profile := os.Getenv("HEYMAN_PROFILE"); profile != "" {
		cfg.DefaultProfile = profile
	}

	return cfg, nil
}

// Save writes the configuration to the config file
func Save(cfg *Config) error {
	configPath := getConfigPath()

	// Ensure config directory exists
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Marshal to TOML
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write to file
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// GetActiveProfile returns the active profile based on flags, env vars, and config
func (c *Config) GetActiveProfile() (*Profile, error) {
	var profileName string

	// Priority: CLI flag > env var > config default
	if viper.IsSet("profile") {
		profileName = viper.GetString("profile")
	} else if c.DefaultProfile != "" {
		profileName = c.DefaultProfile
	} else {
		return nil, fmt.Errorf("no profile specified and no default profile set")
	}

	profile, ok := c.Profiles[profileName]
	if !ok {
		return nil, fmt.Errorf("profile %q not found", profileName)
	}

	profile.Name = profileName
	return &profile, nil
}

// AddProfile adds or updates a profile
func (c *Config) AddProfile(name string, profile Profile) {
	if c.Profiles == nil {
		c.Profiles = make(map[string]Profile)
	}
	profile.Name = name
	c.Profiles[name] = profile
}

// GetContextWindow returns the context window for a profile, defaulting to 8192
func (p *Profile) GetContextWindow() int {
	if p.ContextWindow > 0 {
		return p.ContextWindow
	}
	return 8192 // Default context window
}

// GetAPIKey returns the API key for the given provider
// Checks environment variables first, then profile options
func (c *Config) GetAPIKey(provider string) string {
	// Check provider-specific environment variables
	switch provider {
	case "openai":
		if key := os.Getenv("OPENAI_API_KEY"); key != "" {
			return key
		}
	case "anthropic":
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			return key
		}
	}

	// Ollama doesn't need API key
	return ""
}

// GetOllamaHost returns the Ollama host URL
func GetOllamaHost() string {
	if host := os.Getenv("OLLAMA_HOST"); host != "" {
		return host
	}
	return "http://localhost:11434"
}

// getConfigPath returns the path to the config file
func getConfigPath() string {
	configPath, err := xdg.ConfigFile("heyman/config.toml")
	if err != nil {
		// Fallback to home directory
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "heyman", "config.toml")
	}
	return configPath
}

// GetCacheDir returns the cache directory path
func GetCacheDir() string {
	if cacheDir := os.Getenv("HEYMAN_CACHE_DIR"); cacheDir != "" {
		return cacheDir
	}

	cacheDir, err := xdg.CacheFile("heyman")
	if err != nil {
		// Fallback to home directory
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".cache", "heyman")
	}
	return cacheDir
}
