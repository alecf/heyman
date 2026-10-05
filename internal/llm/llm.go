// Package llm resolves "provider/model" specs into fantasy language models.
//
// A spec looks like "anthropic/claude-haiku-4-5", "ollama/qwen3:0.6b" or
// "openrouter/moonshotai/kimi-k2". Everything before the first "/" is the
// provider; the rest is passed to the provider untouched. Bare model names are
// accepted when the provider is obvious ("claude-…", "gpt-…", "gemini-…", or an
// Ollama-style "name:tag").
package llm

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
)

// DefaultModel is used when no model or profile is configured.
const DefaultModel = "anthropic/claude-haiku-4-5"

// Provider names.
const (
	Anthropic    = "anthropic"
	OpenAI       = "openai"
	OpenRouter   = "openrouter"
	Google       = "google"
	Ollama       = "ollama"
	OpenAICompat = "openai-compat"
	// ClaudeCode shells out to the `claude` CLI instead of calling an API.
	// It is not a fantasy provider; callers handle it separately.
	ClaudeCode = "claude-code"
)

// providers maps each provider to the env vars that may hold its API key, in
// priority order.
var providers = map[string][]string{
	Anthropic:    {"ANTHROPIC_API_KEY", "CLAUDE_API_KEY"},
	OpenAI:       {"OPENAI_API_KEY"},
	OpenRouter:   {"OPENROUTER_API_KEY"},
	Google:       {"GEMINI_API_KEY", "GOOGLE_API_KEY"},
	Ollama:       nil,
	OpenAICompat: {"HEYMAN_OPENAI_COMPAT_API_KEY"},
	ClaudeCode:   nil,
}

// Providers returns the supported provider names, sorted.
func Providers() []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// APIKeyEnv returns the primary environment variable for the provider's API
// key, or "" if the provider doesn't need one.
func APIKeyEnv(provider string) string {
	if envs := providers[provider]; len(envs) > 0 {
		return envs[0]
	}
	return ""
}

// APIKey returns the provider's API key from the environment, or "".
func APIKey(provider string) string {
	for _, env := range providers[provider] {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return ""
}

// Spec identifies a model at a provider.
type Spec struct {
	Provider string
	Model    string
}

func (s Spec) String() string { return s.Provider + "/" + s.Model }

// ParseSpec parses "provider/model", inferring the provider for bare names.
func ParseSpec(s string) (Spec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Spec{}, fmt.Errorf("empty model")
	}
	if p, m, ok := strings.Cut(s, "/"); ok {
		if _, known := providers[p]; known {
			if m == "" {
				return Spec{}, fmt.Errorf("model %q: missing model name after %q", s, p+"/")
			}
			return Spec{Provider: p, Model: m}, nil
		}
	}
	switch {
	case strings.HasPrefix(s, "claude-"):
		return Spec{Anthropic, s}, nil
	case strings.HasPrefix(s, "gpt-"), strings.HasPrefix(s, "chatgpt-"),
		len(s) > 1 && s[0] == 'o' && s[1] >= '0' && s[1] <= '9':
		return Spec{OpenAI, s}, nil
	case strings.HasPrefix(s, "gemini-"):
		return Spec{Google, s}, nil
	case strings.Contains(s, ":") && !strings.Contains(s, "/"):
		return Spec{Ollama, s}, nil
	}
	return Spec{}, fmt.Errorf("can't tell which provider serves %q; use provider/model (providers: %s)",
		s, strings.Join(Providers(), ", "))
}

// Options override provider defaults.
type Options struct {
	APIKey  string // defaults to the provider's env var
	BaseURL string // for ollama / openai-compat (or to point a provider at a proxy)
}

// NewLanguageModel builds a fantasy model for spec.
func NewLanguageModel(ctx context.Context, spec Spec, opts Options) (fantasy.LanguageModel, error) {
	key := opts.APIKey
	if key == "" {
		key = APIKey(spec.Provider)
	}
	needKey := func() error {
		if key == "" {
			return fmt.Errorf("%s requires an API key: set %s", spec.Provider, APIKeyEnv(spec.Provider))
		}
		return nil
	}

	var (
		p   fantasy.Provider
		err error
	)
	switch spec.Provider {
	case Anthropic:
		if err := needKey(); err != nil {
			return nil, err
		}
		o := []anthropic.Option{anthropic.WithAPIKey(key)}
		if opts.BaseURL != "" {
			o = append(o, anthropic.WithBaseURL(opts.BaseURL))
		}
		p, err = anthropic.New(o...)
	case OpenAI:
		if err := needKey(); err != nil {
			return nil, err
		}
		o := []openai.Option{openai.WithAPIKey(key)}
		if opts.BaseURL != "" {
			o = append(o, openai.WithBaseURL(opts.BaseURL))
		}
		p, err = openai.New(o...)
	case OpenRouter:
		if err := needKey(); err != nil {
			return nil, err
		}
		p, err = openrouter.New(openrouter.WithAPIKey(key))
	case Google:
		if err := needKey(); err != nil {
			return nil, err
		}
		p, err = google.New(google.WithGeminiAPIKey(key))
	case Ollama:
		base := opts.BaseURL
		if base == "" {
			base = OllamaBaseURL()
		}
		// Ollama ignores the key, but the OpenAI client insists on one.
		p, err = openaicompat.New(openaicompat.WithName(Ollama), openaicompat.WithBaseURL(base), openaicompat.WithAPIKey("ollama"))
	case OpenAICompat:
		base := opts.BaseURL
		if base == "" {
			base = os.Getenv("HEYMAN_OPENAI_COMPAT_BASE_URL")
		}
		if base == "" {
			return nil, fmt.Errorf("openai-compat needs a base URL: set base_url in the profile or HEYMAN_OPENAI_COMPAT_BASE_URL")
		}
		if key == "" {
			key = "unused"
		}
		p, err = openaicompat.New(openaicompat.WithBaseURL(base), openaicompat.WithAPIKey(key))
	case ClaudeCode:
		return nil, fmt.Errorf("claude-code is not an API provider")
	default:
		return nil, fmt.Errorf("unknown provider %q (providers: %s)", spec.Provider, strings.Join(Providers(), ", "))
	}
	if err != nil {
		return nil, fmt.Errorf("creating %s provider: %w", spec.Provider, err)
	}
	return p.LanguageModel(ctx, spec.Model)
}

// OllamaBaseURL returns the OpenAI-compatible endpoint for the local Ollama
// server, honouring OLLAMA_HOST (which may omit the scheme, e.g. "0.0.0.0:11434").
func OllamaBaseURL() string {
	host := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if host == "" {
		host = "http://localhost:11434"
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	if u, err := url.Parse(host); err == nil && u.Hostname() == "0.0.0.0" {
		u.Host = strings.Replace(u.Host, "0.0.0.0", "localhost", 1)
		host = u.String()
	}
	return strings.TrimRight(host, "/") + "/v1"
}
