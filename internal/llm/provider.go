package llm

import "context"

// Provider is the common interface for all LLM providers
type Provider interface {
	// Query sends a prompt and returns the complete response
	Query(ctx context.Context, req QueryRequest) (*QueryResponse, error)

	// StreamQuery sends a prompt and streams the response
	// Returns two channels: one for chunks, one for errors
	StreamQuery(ctx context.Context, req QueryRequest) (<-chan StreamChunk, <-chan error)

	// GetAvailableModels returns the list of models available from this provider
	GetAvailableModels(ctx context.Context) ([]Model, error)

	// Name returns the provider name (openai, anthropic, ollama)
	Name() string

	// SupportsStreaming indicates if this provider supports streaming
	SupportsStreaming() bool
}

// Model represents an available LLM model
type Model struct {
	ID          string  // e.g., "gpt-4o", "claude-sonnet-4"
	DisplayName string  // Human-readable name
	Provider    string  // Provider name
	Pricing     *Pricing
}

// Pricing represents the cost structure for a model
type Pricing struct {
	InputPerMillionTokens  float64 // Cost per 1M input tokens
	OutputPerMillionTokens float64 // Cost per 1M output tokens
}

// QueryRequest represents a request to an LLM
type QueryRequest struct {
	Model          string
	SystemPrompt   string
	UserPrompt     string
	MaxTokens      int
	Temperature    float64
	ContextWindow  int      // Max context window in tokens
	StopSequences  []string
}

// QueryResponse represents a response from an LLM
type QueryResponse struct {
	Content      string
	TokensInput  int
	TokensOutput int
	Model        string
	Provider     string
	Cached       bool // Whether this was served from cache
}

// StreamChunk represents a chunk of streaming response
type StreamChunk struct {
	Content      string
	IsComplete   bool
	TokensInput  int    // Only populated on completion
	TokensOutput int    // Only populated on completion
}
