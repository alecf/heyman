package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/alecf/heyman/internal/config"
	"github.com/alecf/heyman/internal/llm"
	"github.com/alecf/heyman/internal/spinner"
)

// QueryOptions holds options for executing a query
type QueryOptions struct {
	ShowProgress bool
	Verbose      bool
	Debug        bool
	Profile      *config.Profile
}

// ExecuteQuery sends a query to the LLM provider with appropriate progress indicators
func ExecuteQuery(ctx context.Context, provider llm.Provider, req llm.QueryRequest, opts QueryOptions) (*llm.QueryResponse, error) {
	if opts.ShowProgress {
		// Use streaming with progress indicators
		return executeStreamingQuery(ctx, provider, req, opts)
	}

	// Use non-streaming query for verbose/debug modes
	return provider.Query(ctx, req)
}

func executeStreamingQuery(ctx context.Context, provider llm.Provider, req llm.QueryRequest, opts QueryOptions) (*llm.QueryResponse, error) {
	spin := spinner.New(fmt.Sprintf("Sending query to %s...", opts.Profile.Model))
	spin.Start()

	chunkCh, errCh := provider.StreamQuery(ctx, req)

	var content strings.Builder
	var tokenInput, tokenOutput int
	firstChunk := true

	// Process stream
streamLoop:
	for {
		select {
		case chunk, ok := <-chunkCh:
			if !ok {
				break streamLoop
			}

			if firstChunk && !chunk.IsComplete {
				firstChunk = false
				spin.Update(fmt.Sprintf("Getting command from %s...", opts.Profile.Model))
			}

			if chunk.IsComplete {
				tokenInput = chunk.TokensInput
				tokenOutput = chunk.TokensOutput
				break streamLoop
			}

			content.WriteString(chunk.Content)

		case err := <-errCh:
			spin.Stop()
			return nil, fmt.Errorf("LLM query failed: %w", err)
		}
	}

	spin.Stop()

	return &llm.QueryResponse{
		Content:      content.String(),
		TokensInput:  tokenInput,
		TokensOutput: tokenOutput,
		Model:        req.Model,
		Provider:     provider.Name(),
		Cached:       false,
	}, nil
}
