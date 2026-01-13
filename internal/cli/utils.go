package cli

import (
	"fmt"

	tiktoken "github.com/pkoukk/tiktoken-go"
)

// countTokens estimates token count using tiktoken, falling back to character count
func countTokens(text string, verbose bool) int {
	// Try tiktoken (accurate for OpenAI, decent estimate for Mistral/Llama)
	tke, err := tiktoken.GetEncoding("cl100k_base")
	if err == nil {
		tokens := tke.Encode(text, nil, nil)
		count := len(tokens)
		if verbose {
			fmt.Printf("Prompt tokens: ~%d (tiktoken estimate)\n", count)
		}
		return count
	}

	// Fallback: character count
	count := len(text) / 4
	if verbose {
		fmt.Printf("Prompt tokens: ~%d (character estimate)\n", count)
	}
	return count
}

// truncate truncates a string to maxLen characters with ellipsis
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
