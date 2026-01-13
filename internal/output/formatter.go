package output

import (
	"encoding/json"
	"fmt"

	"github.com/alecf/heyman/internal/llm"
	"github.com/alecf/heyman/internal/parser"
)

// JSONOutput represents the JSON output format
type JSONOutput struct {
	Command     string              `json:"command"`
	Explanation string              `json:"explanation,omitempty"`
	Metadata    *Metadata           `json:"metadata,omitempty"`
}

// Metadata represents metadata about the query
type Metadata struct {
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	TokensInput  int     `json:"tokens_input"`
	TokensOutput int     `json:"tokens_output"`
	Cached       bool    `json:"cached"`
	Cost         *float64 `json:"cost,omitempty"` // nil for Ollama
}

// FormatJSON formats the output as JSON
func FormatJSON(parsed parser.ParsedResponse, resp *llm.QueryResponse, cost *float64) (string, error) {
	output := JSONOutput{
		Command:     parsed.Command,
		Explanation: parsed.Explanation,
		Metadata: &Metadata{
			Provider:     resp.Provider,
			Model:        resp.Model,
			TokensInput:  resp.TokensInput,
			TokensOutput: resp.TokensOutput,
			Cached:       resp.Cached,
			Cost:         cost,
		},
	}

	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal JSON: %w", err)
	}

	return string(data), nil
}

// FormatPlain formats the output as plain text
func FormatPlain(parsed parser.ParsedResponse) string {
	result := parsed.Command
	if parsed.Explanation != "" {
		result += "\n\n" + parsed.Explanation
	}
	return result
}
