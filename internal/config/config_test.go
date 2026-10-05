package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "heyman", "config.toml")
	t.Setenv("HEYMAN_CONFIG", path)
	t.Setenv("HEYMAN_PROFILE", "")
	if body != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestLoadMissing(t *testing.T) {
	setConfig(t, "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CacheDays != 30 || cfg.Profiles == nil || cfg.DefaultProfile != "" {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestLoadMalformed(t *testing.T) {
	path := setConfig(t, "default_profile = [oops")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want mention of %s", err, path)
	}
}

func TestLoadUnreadable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read anything")
	}
	path := setConfig(t, "cache_days = 1")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("unreadable config silently ignored")
	}
}

const sample = `default_profile = 'ollama-gemma3n'
cache_days = 7

[profiles.gemma3n]
provider = 'ollama'
model = 'gemma3n:e2b'
context_window = 8192

[profiles.claude]
provider = 'anthropic'
model = 'claude-haiku-4-5'
`

func TestLoadAndActiveProfile(t *testing.T) {
	setConfig(t, sample)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CacheDays != 7 || cfg.Profiles["gemma3n"].Name != "gemma3n" {
		t.Errorf("cfg = %+v", cfg)
	}
	name, src := cfg.ActiveProfile()
	if name != "ollama-gemma3n" || !strings.Contains(src, "default_profile") {
		t.Errorf("ActiveProfile = %q, %q", name, src)
	}
	t.Setenv("HEYMAN_PROFILE", "claude")
	if name, src := cfg.ActiveProfile(); name != "claude" || src != "HEYMAN_PROFILE" {
		t.Errorf("env ActiveProfile = %q, %q", name, src)
	}
	// HEYMAN_PROFILE must not leak into the file on Save.
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEYMAN_PROFILE", "")
	cfg2, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.DefaultProfile != "ollama-gemma3n" {
		t.Errorf("default_profile after save = %q", cfg2.DefaultProfile)
	}
}

func TestSave(t *testing.T) {
	path := setConfig(t, "")
	cfg, _ := Load()
	cfg.AddProfile("x", Profile{Provider: "ollama", Model: "qwen3:4b", BaseURL: "http://h:1/v1"})
	if err := cfg.SetDefault("x"); err != nil {
		t.Fatal(err)
	}
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o", info.Mode().Perm())
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if p := got.Profiles["x"]; p.BaseURL != "http://h:1/v1" || p.Spec() != "ollama/qwen3:4b" || got.DefaultProfile != "x" {
		t.Errorf("round trip = %+v", got)
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Errorf("leftover files: %v", ents)
	}
}

func TestProfileOps(t *testing.T) {
	cfg := &Config{}
	cfg.AddProfile("b", Profile{Provider: "ollama", Model: "m"})
	cfg.AddProfile("a", Profile{Provider: "ollama", Model: "m"})
	if err := cfg.SetDefault("nope"); err == nil {
		t.Error("SetDefault on missing profile")
	}
	_ = cfg.SetDefault("b")
	if nd, err := cfg.DeleteProfile("b"); err != nil || nd != "a" || cfg.DefaultProfile != "a" {
		t.Errorf("delete default: %q %v %q", nd, err, cfg.DefaultProfile)
	}
	if nd, err := cfg.DeleteProfile("a"); err != nil || nd != "" || cfg.DefaultProfile != "" {
		t.Errorf("delete last: %q %v", nd, err)
	}
	if _, err := cfg.DeleteProfile("a"); err == nil {
		t.Error("delete missing")
	}
	p := Profile{Name: "x", Provider: "ollama"}
	if err := p.Validate(); err == nil {
		t.Error("profile without model validated")
	}
}

func TestGetCacheDir(t *testing.T) {
	t.Setenv("HEYMAN_CACHE_DIR", "/tmp/x")
	if GetCacheDir() != "/tmp/x" {
		t.Error("HEYMAN_CACHE_DIR ignored")
	}
	t.Setenv("HEYMAN_CACHE_DIR", "")
	if !strings.HasSuffix(GetCacheDir(), "heyman") {
		t.Errorf("GetCacheDir = %q", GetCacheDir())
	}
}
