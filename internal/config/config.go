package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/adrg/xdg"
	"github.com/pelletier/go-toml/v2"
)

// Config represents the entire heyman configuration
type Config struct {
	DefaultProfile string             `toml:"default_profile"`
	CacheDays      int                `toml:"cache_days"`
	Profiles       map[string]Profile `toml:"profiles"`
}

// Profile represents an LLM provider configuration
type Profile struct {
	Name     string `toml:"-"`        // Set from map key
	Provider string `toml:"provider"` // see llm.Providers()
	Model    string `toml:"model"`
	// BaseURL overrides the provider endpoint (required for openai-compat,
	// optional for ollama and proxies).
	BaseURL string `toml:"base_url,omitempty"`
	// ContextWindow is deprecated and ignored; kept so old configs still load.
	ContextWindow int            `toml:"context_window,omitempty"`
	Options       map[string]any `toml:"options,omitempty"`
}

// Spec returns the profile's "provider/model" string.
func (p *Profile) Spec() string {
	return p.Provider + "/" + p.Model
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

	// Ensure config directory exists (0700 for security)
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Marshal to TOML
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write to file (0600: profiles may contain endpoints and options)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// AddProfile adds or updates a profile
func (c *Config) AddProfile(name string, profile Profile) {
	if c.Profiles == nil {
		c.Profiles = make(map[string]Profile)
	}
	profile.Name = name
	c.Profiles[name] = profile
}

// DeleteProfile removes a profile and handles default reassignment
// Returns the new default profile name if changed, empty string if unchanged
func (c *Config) DeleteProfile(name string) (newDefault string, err error) {
	if _, ok := c.Profiles[name]; !ok {
		return "", fmt.Errorf("profile %q not found", name)
	}

	delete(c.Profiles, name)

	// Handle default reassignment if we deleted the default
	if c.DefaultProfile == name {
		if len(c.Profiles) == 0 {
			c.DefaultProfile = ""
			return "", nil
		}
		// Pick first alphabetically for predictability
		c.DefaultProfile = c.SortedProfileNames()[0]
		return c.DefaultProfile, nil
	}

	return "", nil
}

// SetDefault sets the default profile
func (c *Config) SetDefault(name string) error {
	if _, ok := c.Profiles[name]; !ok {
		return fmt.Errorf("profile %q not found", name)
	}
	c.DefaultProfile = name
	return nil
}

// ProfileExists checks if a profile name already exists
func (c *Config) ProfileExists(name string) bool {
	_, ok := c.Profiles[name]
	return ok
}

// SortedProfileNames returns profile names in alphabetical order
func (c *Config) SortedProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
