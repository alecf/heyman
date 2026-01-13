package cli

import (
	"context"
	"fmt"

	"github.com/alecf/heyman/internal/config"
	"github.com/alecf/heyman/internal/llm"
)

// ProviderConfig holds the provider and its context window
type ProviderConfig struct {
	Provider      llm.Provider
	ContextWindow int
}

// CreateProvider initializes a provider based on the profile configuration
func CreateProvider(ctx context.Context, cfg *config.Config, profile *config.Profile, verbose bool) (*ProviderConfig, error) {
	var provider llm.Provider
	var contextWindow int
	var err error

	switch profile.Provider {
	case "openai":
		apiKey := cfg.GetAPIKey("openai")
		if apiKey == "" {
			return nil, fmt.Errorf("OpenAI API key not found. Set OPENAI_API_KEY environment variable")
		}
		provider, err = llm.NewOpenAIProvider(apiKey)
		if err != nil {
			return nil, fmt.Errorf("failed to create OpenAI provider: %w", err)
		}
		contextWindow = profile.GetContextWindow()

	case "ollama":
		provider, err = llm.NewOllamaProvider()
		if err != nil {
			return nil, fmt.Errorf("failed to create Ollama provider: %w", err)
		}

		// Auto-detect context window from Ollama if not configured
		if profile.ContextWindow == 0 {
			if ollamaProvider, ok := provider.(*llm.OllamaProvider); ok {
				if ctxWindow, err := ollamaProvider.GetModelContextWindow(ctx, profile.Model); err == nil {
					// Cap at 8k tokens for practical memory/performance reasons
					// Models may advertise larger context windows than Ollama can handle
					const maxPracticalContext = 8192
					if ctxWindow > maxPracticalContext {
						contextWindow = maxPracticalContext
						if verbose {
							fmt.Printf("Auto-detected context window: %d tokens (capped at %d for memory)\n", ctxWindow, maxPracticalContext)
						}
					} else {
						contextWindow = ctxWindow
						if verbose {
							fmt.Printf("Auto-detected context window: %d tokens\n", ctxWindow)
						}
					}
				} else {
					contextWindow = profile.GetContextWindow() // Fallback to default
				}
			} else {
				contextWindow = profile.GetContextWindow()
			}
		} else {
			contextWindow = profile.ContextWindow
		}

	default:
		return nil, fmt.Errorf("unsupported provider: %s", profile.Provider)
	}

	return &ProviderConfig{
		Provider:      provider,
		ContextWindow: contextWindow,
	}, nil
}
