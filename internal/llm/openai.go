package llm

import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// OpenAIProvider implements the Provider interface for OpenAI
type OpenAIProvider struct {
	client openai.Client
}

// NewOpenAIProvider creates a new OpenAI provider
func NewOpenAIProvider(apiKey string) (*OpenAIProvider, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("OpenAI API key is required")
	}

	client := openai.NewClient(
		option.WithAPIKey(apiKey),
	)

	return &OpenAIProvider{
		client: client,
	}, nil
}

// Query sends a non-streaming request to OpenAI
func (p *OpenAIProvider) Query(ctx context.Context, req QueryRequest) (*QueryResponse, error) {
	chatReq := openai.ChatCompletionNewParams{
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(req.SystemPrompt),
			openai.UserMessage(req.UserPrompt),
		},
		Model:       openai.ChatModel(req.Model),
		MaxTokens:   openai.Int(int64(req.MaxTokens)),
		Temperature: openai.Float(req.Temperature),
	}

	resp, err := p.client.Chat.Completions.New(ctx, chatReq)
	if err != nil {
		return nil, fmt.Errorf("OpenAI API error: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("no response from OpenAI")
	}

	return &QueryResponse{
		Content:      resp.Choices[0].Message.Content,
		TokensInput:  int(resp.Usage.PromptTokens),
		TokensOutput: int(resp.Usage.CompletionTokens),
		Model:        req.Model,
		Provider:     "openai",
		Cached:       false,
	}, nil
}

// StreamQuery sends a streaming request to OpenAI
func (p *OpenAIProvider) StreamQuery(ctx context.Context, req QueryRequest) (<-chan StreamChunk, <-chan error) {
	chunkCh := make(chan StreamChunk)
	errCh := make(chan error, 1)

	go func() {
		defer close(chunkCh)
		defer close(errCh)

		chatReq := openai.ChatCompletionNewParams{
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage(req.SystemPrompt),
				openai.UserMessage(req.UserPrompt),
			},
			Model:       openai.ChatModel(req.Model),
			MaxTokens:   openai.Int(int64(req.MaxTokens)),
			Temperature: openai.Float(req.Temperature),
		}

		stream := p.client.Chat.Completions.NewStreaming(ctx, chatReq)

		// Use accumulator to track streaming state
		acc := openai.ChatCompletionAccumulator{}

		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)

			// Send delta content as it arrives
			if len(chunk.Choices) > 0 {
				delta := chunk.Choices[0].Delta
				if delta.Content != "" {
					chunkCh <- StreamChunk{
						Content:    delta.Content,
						IsComplete: false,
					}
				}
			}
		}

		if err := stream.Err(); err != nil {
			errCh <- fmt.Errorf("stream error: %w", err)
			return
		}

		// After streaming, get token counts from accumulator
		inputTokens := int(acc.Usage.PromptTokens)
		outputTokens := int(acc.Usage.CompletionTokens)

		chunkCh <- StreamChunk{
			Content:      "",
			IsComplete:   true,
			TokensInput:  inputTokens,
			TokensOutput: outputTokens,
		}
	}()

	return chunkCh, errCh
}

// GetAvailableModels returns the list of available OpenAI models
func (p *OpenAIProvider) GetAvailableModels(ctx context.Context) ([]Model, error) {
	// For now, return a hardcoded list of common models
	// In the future, we could query the API
	models := []Model{
		{
			ID:          "gpt-4o",
			DisplayName: "GPT-4o",
			Provider:    "openai",
			Pricing: &Pricing{
				InputPerMillionTokens:  2.50,
				OutputPerMillionTokens: 10.00,
			},
		},
		{
			ID:          "gpt-4o-mini",
			DisplayName: "GPT-4o Mini",
			Provider:    "openai",
			Pricing: &Pricing{
				InputPerMillionTokens:  0.15,
				OutputPerMillionTokens: 0.60,
			},
		},
		{
			ID:          "gpt-4-turbo",
			DisplayName: "GPT-4 Turbo",
			Provider:    "openai",
			Pricing: &Pricing{
				InputPerMillionTokens:  10.00,
				OutputPerMillionTokens: 30.00,
			},
		},
	}

	return models, nil
}

// Name returns the provider name
func (p *OpenAIProvider) Name() string {
	return "openai"
}

// SupportsStreaming indicates that OpenAI supports streaming
func (p *OpenAIProvider) SupportsStreaming() bool {
	return true
}
