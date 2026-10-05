package llm

import (
	"context"
	"strings"
	"testing"
)

func TestParseSpec(t *testing.T) {
	tests := []struct {
		in       string
		provider string
		model    string
		wantErr  bool
	}{
		{"anthropic/claude-haiku-4-5", Anthropic, "claude-haiku-4-5", false},
		{"  Anthropic/claude-haiku-4-5 ", Anthropic, "claude-haiku-4-5", false},
		{"claude-x", Anthropic, "claude-x", false},
		{"o3-mini", OpenAI, "o3-mini", false},
		{"o1", OpenAI, "o1", false},
		{"gpt-4o-mini", OpenAI, "gpt-4o-mini", false},
		{"ft:gpt-4o-mini:org::abc", OpenAI, "ft:gpt-4o-mini:org::abc", false},
		{"gemini-2.5-flash", Google, "gemini-2.5-flash", false},
		{"qwen3:4b", Ollama, "qwen3:4b", false},
		{"gpt-oss:20b", Ollama, "gpt-oss:20b", false},
		{"ollama/hf.co/x/y:Q4", Ollama, "hf.co/x/y:Q4", false},
		{"openrouter/a/b", OpenRouter, "a/b", false},
		{"openrouter/moonshotai/kimi-k2:free", OpenRouter, "moonshotai/kimi-k2:free", false},
		{"claude-code/haiku", ClaudeCode, "haiku", false},
		{"openai-compat/local", OpenAICompat, "local", false},
		{"", "", "", true},
		{"anthropic/", "", "", true},
		{"ollama", "", "", true},
		{"claude-code", "", "", true},
		{"gemma3n", "", "", true},
		{"hf.co/x/y:Q4", "", "", true},
		{"unknown/model", "", "", true},
	}
	for _, tt := range tests {
		got, err := ParseSpec(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseSpec(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && (got.Provider != tt.provider || got.Model != tt.model) {
			t.Errorf("ParseSpec(%q) = %+v, want %s/%s", tt.in, got, tt.provider, tt.model)
		}
	}
	if _, err := ParseSpec("gemma3n"); err == nil || !strings.Contains(err.Error(), "ollama/gemma3n") {
		t.Errorf("bare name error should suggest ollama/: %v", err)
	}
}

func TestOllamaBaseURL(t *testing.T) {
	tests := map[string]string{
		"":                              "http://localhost:11434/v1",
		"0.0.0.0:11434":                 "http://localhost:11434/v1",
		"0.0.0.0":                       "http://localhost:11434/v1",
		"http://host:1234/":             "http://host:1234/v1",
		"[::]:11434":                    "http://localhost:11434/v1",
		"[::1]:9999":                    "http://[::1]:9999/v1",
		"myhost":                        "http://myhost:11434/v1",
		":11435":                        "http://localhost:11435/v1",
		"https://ollama.example.com":    "https://ollama.example.com:443/v1",
		"http://example.com:80/ollama/": "http://example.com:80/ollama/v1",
		" 127.0.0.1:11434 ":             "http://127.0.0.1:11434/v1",
	}
	for in, want := range tests {
		if got := ollamaBaseURL(in); got != want {
			t.Errorf("ollamaBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
	t.Setenv("OLLAMA_HOST", "0.0.0.0:1")
	if got := OllamaBaseURL(); got != "http://localhost:1/v1" {
		t.Errorf("OllamaBaseURL() = %q", got)
	}
}

func TestNewLanguageModelKeys(t *testing.T) {
	for _, env := range []string{"ANTHROPIC_API_KEY", "CLAUDE_API_KEY", "OPENAI_API_KEY", "HEYMAN_OPENAI_COMPAT_BASE_URL"} {
		t.Setenv(env, "")
	}
	ctx := context.Background()
	_, err := NewLanguageModel(ctx, Spec{Anthropic, "claude-haiku-4-5"}, Options{})
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY or CLAUDE_API_KEY") {
		t.Errorf("missing key error = %v", err)
	}
	t.Setenv("CLAUDE_API_KEY", "test")
	if _, err := NewLanguageModel(ctx, Spec{Anthropic, "claude-haiku-4-5"}, Options{}); err != nil {
		t.Errorf("CLAUDE_API_KEY not used: %v", err)
	}
	if _, err := NewLanguageModel(ctx, Spec{Ollama, "qwen3:4b"}, Options{}); err != nil {
		t.Errorf("ollama needs no key: %v", err)
	}
	if _, err := NewLanguageModel(ctx, Spec{OpenAICompat, "m"}, Options{}); err == nil {
		t.Error("openai-compat without base URL: want error")
	}
	if _, err := NewLanguageModel(ctx, Spec{ClaudeCode, "haiku"}, Options{}); err == nil {
		t.Error("claude-code is not an API provider")
	}
	if _, err := NewLanguageModel(ctx, Spec{"nope", "m"}, Options{}); err == nil {
		t.Error("unknown provider: want error")
	}
}
