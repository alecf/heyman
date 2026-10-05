package output

import (
	"encoding/json"
	"fmt"

	"github.com/alecf/heyman/internal/assist"
)

// JSONOutput represents the JSON output format
type JSONOutput struct {
	Command     string    `json:"command"`
	Explanation string    `json:"explanation,omitempty"`
	Metadata    *Metadata `json:"metadata,omitempty"`
}

// Metadata represents metadata about the query
type Metadata struct {
	Model        string   `json:"model"`
	ManPages     []string `json:"man_pages,omitempty"`
	Steps        int      `json:"steps"`
	TokensInput  int64    `json:"tokens_input"`
	TokensOutput int64    `json:"tokens_output"`
	Cached       bool     `json:"cached"`
	Cost         *float64 `json:"cost,omitempty"` // nil when unknown or free; 0 when cached
	Notes        []string `json:"notes,omitempty"`
}

// FormatJSON formats the output as JSON
func FormatJSON(res *assist.Result, explain bool, cost *float64) (string, error) {
	out := JSONOutput{
		Command: res.Command,
		Metadata: &Metadata{
			Model:        res.Model,
			ManPages:     res.ManPages,
			Steps:        res.Steps,
			TokensInput:  res.Usage.InputTokens,
			TokensOutput: res.Usage.OutputTokens,
			Cached:       res.Cached,
			Cost:         cost,
			Notes:        res.Notes,
		},
	}
	if explain {
		out.Explanation = res.Explanation
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal JSON: %w", err)
	}

	return string(data), nil
}
