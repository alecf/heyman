package assist

import (
	"context"

	"github.com/alecf/heyman/internal/llm"
	"github.com/alecf/heyman/internal/manpage"
)

// Config selects and configures a backend.
type Config struct {
	Model   string // "provider/model"; empty means llm.DefaultModel
	BaseURL string
	// ReasoningEffort is passed to ollama / openai-compat models ("none"
	// disables thinking).
	ReasoningEffort string
	Man             ManSource
	OnEvent         func(Event)
}

// New returns an Answerer for cfg.Model: a fantasy-backed Assistant for API
// providers, or the claude CLI wrapper for "claude-code/…".
func New(ctx context.Context, cfg Config) (Answerer, llm.Spec, error) {
	if cfg.Man == nil {
		cfg.Man = manpage.NewFetcher()
	}
	model := cfg.Model
	if model == "" {
		model = llm.DefaultModel
	}
	spec, err := llm.ParseSpec(model)
	if err != nil {
		return nil, llm.Spec{}, err
	}
	if spec.Provider == llm.ClaudeCode {
		return &ClaudeCode{Model: spec.Model, Man: cfg.Man, OnEvent: cfg.OnEvent}, spec, nil
	}
	lm, err := llm.NewLanguageModel(ctx, spec, llm.Options{BaseURL: cfg.BaseURL})
	if err != nil {
		return nil, spec, err
	}
	return &Assistant{
		Model:     lm,
		ModelName: spec.String(),
		Man:       cfg.Man,
		OnEvent:   cfg.OnEvent,

		ReasoningEffort: cfg.ReasoningEffort,
	}, spec, nil
}

// PromptPreview returns the system and user prompts Ask would send to a
// tool-calling model, without calling it (for --dry-run and --debug). The
// claude-code backend and the no-tools fallback use a variant without the
// tool instructions.
func PromptPreview(man ManSource, req Request) (system, user string, err error) {
	if man == nil {
		man = manpage.NewFetcher()
	}
	req, preload, missing, err := preparePreload(man, req)
	if err != nil {
		return "", "", err
	}
	return systemPrompt(req, preload, 48000, true, missing), userPrompt(req), nil
}
