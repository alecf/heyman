package llm

import (
	"context"
	"fmt"

	"github.com/ollama/ollama/api"
)

// OllamaProvider implements the Provider interface for Ollama
type OllamaProvider struct {
	client *api.Client
}

// NewOllamaProvider creates a new Ollama provider
func NewOllamaProvider() (*OllamaProvider, error) {
	// Initialize client from environment (OLLAMA_HOST)
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return nil, fmt.Errorf("failed to create Ollama client: %w", err)
	}

	return &OllamaProvider{
		client: client,
	}, nil
}

// Query sends a non-streaming request to Ollama
func (p *OllamaProvider) Query(ctx context.Context, req QueryRequest) (*QueryResponse, error) {
	messages := []api.Message{
		{
			Role:    "system",
			Content: req.SystemPrompt,
		},
		{
			Role:    "user",
			Content: req.UserPrompt,
		},
	}

	// Use configured context window, default to 8192 if not set
	contextWindow := req.ContextWindow
	if contextWindow == 0 {
		contextWindow = 8192
	}

	chatReq := &api.ChatRequest{
		Model:    req.Model,
		Messages: messages,
		Options: map[string]interface{}{
			"temperature":   req.Temperature,
			"num_predict":   req.MaxTokens,
			"num_ctx":       contextWindow,
		},
	}

	// Accumulate response content
	var fullContent string
	var promptTokens, completionTokens int

	respFunc := func(resp api.ChatResponse) error {
		fullContent += resp.Message.Content

		// Capture token counts from final response
		if resp.Done {
			promptTokens = resp.PromptEvalCount
			completionTokens = resp.EvalCount
		}
		return nil
	}

	err := p.client.Chat(ctx, chatReq, respFunc)
	if err != nil {
		return nil, fmt.Errorf("Ollama API error: %w", err)
	}

	return &QueryResponse{
		Content:      fullContent,
		TokensInput:  promptTokens,
		TokensOutput: completionTokens,
		Model:        req.Model,
		Provider:     "ollama",
		Cached:       false,
	}, nil
}

// StreamQuery sends a streaming request to Ollama
func (p *OllamaProvider) StreamQuery(ctx context.Context, req QueryRequest) (<-chan StreamChunk, <-chan error) {
	chunkCh := make(chan StreamChunk)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunkCh)
		defer close(errCh)

		messages := []api.Message{
			{
				Role:    "system",
				Content: req.SystemPrompt,
			},
			{
				Role:    "user",
				Content: req.UserPrompt,
			},
		}

		// Use configured context window, default to 8192 if not set
		contextWindow := req.ContextWindow
		if contextWindow == 0 {
			contextWindow = 8192
		}

		chatReq := &api.ChatRequest{
			Model:    req.Model,
			Messages: messages,
			Options: map[string]interface{}{
				"temperature":   req.Temperature,
				"num_predict":   req.MaxTokens,
				"num_ctx":       contextWindow,
			},
		}

		var promptTokens, completionTokens int

		respFunc := func(resp api.ChatResponse) error {
			if resp.Done {
				// Final response with token counts
				promptTokens = resp.PromptEvalCount
				completionTokens = resp.EvalCount

				chunkCh <- StreamChunk{
					Content:      "",
					IsComplete:   true,
					TokensInput:  promptTokens,
					TokensOutput: completionTokens,
				}
			} else {
				// Stream content chunks
				if resp.Message.Content != "" {
					chunkCh <- StreamChunk{
						Content:    resp.Message.Content,
						IsComplete: false,
					}
				}
			}
			return nil
		}

		err := p.client.Chat(ctx, chatReq, respFunc)
		if err != nil {
			errCh <- fmt.Errorf("stream error: %w", err)
			return
		}
	}()

	return chunkCh, errCh
}

// GetAvailableModels returns the list of available Ollama models
func (p *OllamaProvider) GetAvailableModels(ctx context.Context) ([]Model, error) {
	listResp, err := p.client.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list Ollama models: %w", err)
	}

	models := make([]Model, 0, len(listResp.Models))
	for _, m := range listResp.Models {
		models = append(models, Model{
			ID:          m.Name,
			DisplayName: m.Name,
			Provider:    "ollama",
			Pricing:     nil, // Ollama is free
		})
	}

	return models, nil
}

// Name returns the provider name
func (p *OllamaProvider) Name() string {
	return "ollama"
}

// SupportsStreaming indicates that Ollama supports streaming
func (p *OllamaProvider) SupportsStreaming() bool {
	return true
}

// GetModelContextWindow queries Ollama for the model's context window size
func (p *OllamaProvider) GetModelContextWindow(ctx context.Context, modelName string) (int, error) {
	showReq := &api.ShowRequest{
		Name: modelName,
	}

	showResp, err := p.client.Show(ctx, showReq)
	if err != nil {
		return 0, fmt.Errorf("failed to get model info: %w", err)
	}

	// Look for context_length in model_info
	modelInfo := showResp.ModelInfo
	// Check different possible field names for context length
	for _, key := range []string{
		"mistral3.context_length",
		"llama.context_length",
		"context_length",
	} {
		if val, ok := modelInfo[key]; ok {
			if ctxLen, ok := val.(float64); ok {
				return int(ctxLen), nil
			}
		}
	}

	return 0, fmt.Errorf("context_length not found in model info")
}
