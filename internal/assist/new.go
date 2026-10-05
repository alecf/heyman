package assist

import (
	"context"

	"github.com/alecf/heyman/internal/llm"
)

// Config selects and configures a backend.
type Config struct {
	Model   string // "provider/model"; empty means llm.DefaultModel
	BaseURL string
	Man     ManSource
	OnEvent func(Event)
}

// New returns an Answerer for cfg.Model: a fantasy-backed Assistant for API
// providers, or the claude CLI wrapper for "claude-code/…".
func New(ctx context.Context, cfg Config) (Answerer, llm.Spec, error) {
	model := cfg.Model
	if model == "" {
		model = llm.DefaultModel
	}
	spec, err := llm.ParseSpec(model)
	if err != nil {
		return nil, llm.Spec{}, err
	}
	if spec.Provider == llm.ClaudeCode {
		return &ClaudeCode{Model: spec.Model, Man: cfg.Man}, spec, nil
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
	}, spec, nil
}

// PromptPreview returns the system and user prompts Ask would send, without
// calling a model (for --dry-run).
func PromptPreview(man ManSource, req Request) (system, user string, err error) {
	var preload string
	if req.Command != "" {
		preload, err = man.Fetch(req.Command, req.Section)
		if err != nil {
			return "", "", err
		}
	}
	return systemPrompt(req, preload, 48000, true), userPrompt(req), nil
}
