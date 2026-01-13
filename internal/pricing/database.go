package pricing

import (
	"fmt"
	"time"
)

// Database represents pricing information for LLM models
type Database struct {
	LastUpdated time.Time
	Models      map[string]*ModelPricing
}

// ModelPricing represents pricing for a specific model
type ModelPricing struct {
	Provider           string
	Model              string
	InputPerMillion    float64 // Cost per 1M input tokens
	OutputPerMillion   float64 // Cost per 1M output tokens
	PricingURL         string  // URL to current pricing page
}

// GetDatabase returns the embedded pricing database
func GetDatabase() *Database {
	// Last updated: 2026-01-12
	return &Database{
		LastUpdated: time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC),
		Models: map[string]*ModelPricing{
			// OpenAI Models
			"gpt-4o": {
				Provider:         "openai",
				Model:            "gpt-4o",
				InputPerMillion:  2.50,
				OutputPerMillion: 10.00,
				PricingURL:       "https://openai.com/api/pricing/",
			},
			"gpt-4o-mini": {
				Provider:         "openai",
				Model:            "gpt-4o-mini",
				InputPerMillion:  0.15,
				OutputPerMillion: 0.60,
				PricingURL:       "https://openai.com/api/pricing/",
			},
			"gpt-4-turbo": {
				Provider:         "openai",
				Model:            "gpt-4-turbo",
				InputPerMillion:  10.00,
				OutputPerMillion: 30.00,
				PricingURL:       "https://openai.com/api/pricing/",
			},
			"gpt-4": {
				Provider:         "openai",
				Model:            "gpt-4",
				InputPerMillion:  30.00,
				OutputPerMillion: 60.00,
				PricingURL:       "https://openai.com/api/pricing/",
			},
			"gpt-3.5-turbo": {
				Provider:         "openai",
				Model:            "gpt-3.5-turbo",
				InputPerMillion:  0.50,
				OutputPerMillion: 1.50,
				PricingURL:       "https://openai.com/api/pricing/",
			},

			// Anthropic Models
			"claude-opus-4-5-20251101": {
				Provider:         "anthropic",
				Model:            "claude-opus-4-5-20251101",
				InputPerMillion:  15.00,
				OutputPerMillion: 75.00,
				PricingURL:       "https://www.anthropic.com/pricing",
			},
			"claude-sonnet-4-5-20250924": {
				Provider:         "anthropic",
				Model:            "claude-sonnet-4-5-20250924",
				InputPerMillion:  3.00,
				OutputPerMillion: 15.00,
				PricingURL:       "https://www.anthropic.com/pricing",
			},
			"claude-3-5-sonnet-20241022": {
				Provider:         "anthropic",
				Model:            "claude-3-5-sonnet-20241022",
				InputPerMillion:  3.00,
				OutputPerMillion: 15.00,
				PricingURL:       "https://www.anthropic.com/pricing",
			},
			"claude-3-5-haiku-20241022": {
				Provider:         "anthropic",
				Model:            "claude-3-5-haiku-20241022",
				InputPerMillion:  0.80,
				OutputPerMillion: 4.00,
				PricingURL:       "https://www.anthropic.com/pricing",
			},
		},
	}
}

// GetPricing returns pricing for a specific model
func (db *Database) GetPricing(model string) *ModelPricing {
	return db.Models[model]
}

// CalculateCost calculates the cost for a given number of input and output tokens
func (mp *ModelPricing) CalculateCost(inputTokens, outputTokens int) float64 {
	inputCost := float64(inputTokens) / 1_000_000.0 * mp.InputPerMillion
	outputCost := float64(outputTokens) / 1_000_000.0 * mp.OutputPerMillion
	return inputCost + outputCost
}

// FormatCost formats cost with disclaimer
func (mp *ModelPricing) FormatCost(inputTokens, outputTokens int, lastUpdated time.Time) string {
	cost := mp.CalculateCost(inputTokens, outputTokens)

	warning := fmt.Sprintf("$%.4f (estimated, based on %s pricing)\n\n",
		cost, lastUpdated.Format("2006-01-02"))
	warning += fmt.Sprintf("⚠️  Pricing may have changed. Check current rates:\n")
	warning += fmt.Sprintf("    %s", mp.PricingURL)

	return warning
}

// FormatTokenUsage formats token usage information
func FormatTokenUsage(inputTokens, outputTokens int, mp *ModelPricing, lastUpdated time.Time) string {
	result := "Token usage:\n"
	result += fmt.Sprintf("  Input:  %s tokens\n", formatNumber(inputTokens))
	result += fmt.Sprintf("  Output: %s tokens\n", formatNumber(outputTokens))
	result += fmt.Sprintf("  Total:  %s tokens\n", formatNumber(inputTokens+outputTokens))

	if mp != nil {
		result += fmt.Sprintf("  Cost:   %s", mp.FormatCost(inputTokens, outputTokens, lastUpdated))
	} else {
		result += "  Cost:   Free (Ollama)"
	}

	return result
}

// formatNumber adds commas to large numbers
func formatNumber(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}

	str := fmt.Sprintf("%d", n)
	result := ""
	for i, c := range str {
		if i > 0 && (len(str)-i)%3 == 0 {
			result += ","
		}
		result += string(c)
	}
	return result
}
