package config

import (
	"errors"
	"fmt"
	"io/fs"
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

// Validate reports whether the profile names a provider and a model.
func (p *Profile) Validate() error {
	if p.Provider == "" || p.Model == "" {
		return fmt.Errorf("profile %q needs both provider and model (have provider=%q model=%q)", p.Name, p.Provider, p.Model)
	}
	return nil
}

// Load reads the configuration file. A missing file is not an error: the
// defaults are returned. HEYMAN_PROFILE is not applied here (see
// ActiveProfile) so that Save never writes an environment override back to
// the file.
func Load() (*Config, error) {
	cfg := &Config{
		CacheDays: 30,
		Profiles:  make(map[string]Profile),
	}

	configPath := Path()
	data, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading %s: %w", configPath, err)
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", configPath, err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]Profile)
	}
	// Set profile names from map keys
	for name, profile := range cfg.Profiles {
		profile.Name = name
		cfg.Profiles[name] = profile
	}
	return cfg, nil
}

// ActiveProfile returns the profile to use when none is given on the command
// line: HEYMAN_PROFILE if set, else default_profile. source describes where
// the name came from, for error messages. name is "" if neither is set.
func (c *Config) ActiveProfile() (name, source string) {
	if p := os.Getenv("HEYMAN_PROFILE"); p != "" {
		return p, "HEYMAN_PROFILE"
	}
	if c.DefaultProfile != "" {
		return c.DefaultProfile, "default_profile in " + Path()
	}
	return "", ""
}

// Save writes the configuration to the config file atomically (temp file +
// rename) with 0600 permissions.
func Save(cfg *Config) error {
	configPath := Path()

	// Ensure config directory exists (0700 for security)
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	tmp, err := os.CreateTemp(configDir, ".config.toml.*.tmp")
	if err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write config file: %w", err)
	}
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	if err := os.Rename(tmp.Name(), configPath); err != nil {
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

// Path returns the config file path: $HEYMAN_CONFIG if set, else
// <XDG config home>/heyman/config.toml (~/Library/Application Support on
// macOS). Unlike xdg.ConfigFile it does not create any directories.
func Path() string {
	if p := os.Getenv("HEYMAN_CONFIG"); p != "" {
		return p
	}
	const rel = "heyman/config.toml"
	if xdg.ConfigHome != "" {
		return filepath.Join(xdg.ConfigHome, rel)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", rel)
}

// GetCacheDir returns the cache directory path
func GetCacheDir() string {
	if cacheDir := os.Getenv("HEYMAN_CACHE_DIR"); cacheDir != "" {
		return cacheDir
	}

	if xdg.CacheHome != "" {
		return filepath.Join(xdg.CacheHome, "heyman")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "heyman")
}
