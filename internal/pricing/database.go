package pricing

import (
	"fmt"
	"strings"
	"time"
)

// Database represents pricing information for LLM models
type Database struct {
	LastUpdated time.Time
	Models      map[string]*ModelPricing // keyed by "provider/model"
}

// ModelPricing represents pricing for a specific model
type ModelPricing struct {
	Provider         string
	Model            string
	InputPerMillion  float64 // Cost per 1M input tokens
	OutputPerMillion float64 // Cost per 1M output tokens
	PricingURL       string  // URL to current pricing page
}

const anthropicPricingURL = "https://www.anthropic.com/pricing#api"

func anthropicModel(model string, in, out float64) *ModelPricing {
	return &ModelPricing{Provider: "anthropic", Model: model, InputPerMillion: in, OutputPerMillion: out, PricingURL: anthropicPricingURL}
}

var database = &Database{
	LastUpdated: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
	Models: map[string]*ModelPricing{
		"anthropic/claude-haiku-4-5":  anthropicModel("claude-haiku-4-5", 1.00, 5.00),
		"anthropic/claude-sonnet-5-5": anthropicModel("claude-sonnet-5-5", 2.00, 10.00),
		"anthropic/claude-sonnet-5":   anthropicModel("claude-sonnet-5", 2.00, 10.00),
		"anthropic/claude-sonnet-4-6": anthropicModel("claude-sonnet-4-6", 3.00, 15.00),
		"anthropic/claude-opus-5-5":   anthropicModel("claude-opus-5-5", 4.00, 20.00),
		"anthropic/claude-opus-5":     anthropicModel("claude-opus-5", 5.00, 25.00),
		"anthropic/claude-opus-4-8":   anthropicModel("claude-opus-4-8", 5.00, 25.00),
		"anthropic/claude-fable-5-1":  anthropicModel("claude-fable-5-1", 10.00, 50.00),
	},
}

// GetDatabase returns the embedded pricing database
func GetDatabase() *Database {
	return database
}

// GetPricing returns pricing for a "provider/model" spec, or nil if unknown.
// Dated snapshots ("claude-haiku-4-5-20251001") match their alias.
func (db *Database) GetPricing(spec string) *ModelPricing {
	if mp, ok := db.Models[spec]; ok {
		return mp
	}
	// Longest prefix wins: "claude-opus-5-5-20260101" must match
	// "claude-opus-5-5", not "claude-opus-5" (map order is random).
	var best *ModelPricing
	bestLen := 0
	for key, mp := range db.Models {
		if len(key) > bestLen && strings.HasPrefix(spec, key+"-") {
			best, bestLen = mp, len(key)
		}
	}
	return best
}

// IsFree reports whether the provider runs models locally at no API cost.
func IsFree(provider string) bool {
	return provider == "ollama"
}

// CalculateCost calculates the cost for a given number of input and output tokens
func (mp *ModelPricing) CalculateCost(inputTokens, outputTokens int64) float64 {
	inputCost := float64(inputTokens) / 1_000_000.0 * mp.InputPerMillion
	outputCost := float64(outputTokens) / 1_000_000.0 * mp.OutputPerMillion
	return inputCost + outputCost
}

// FormatTokenUsage formats token usage information. cost is the known cost
// (nil if unknown); free marks local models.
func FormatTokenUsage(inputTokens, outputTokens int64, cost *float64, free bool, mp *ModelPricing, lastUpdated time.Time) string {
	var b strings.Builder
	b.WriteString("Token usage:\n")
	fmt.Fprintf(&b, "  Input:  %s tokens\n", formatNumber(inputTokens))
	fmt.Fprintf(&b, "  Output: %s tokens\n", formatNumber(outputTokens))
	fmt.Fprintf(&b, "  Total:  %s tokens\n", formatNumber(inputTokens+outputTokens))
	switch {
	case free:
		b.WriteString("  Cost:   free (local model)")
	case cost != nil && mp != nil:
		fmt.Fprintf(&b, "  Cost:   $%.4f (estimated from %s list prices; check %s)", *cost, lastUpdated.Format("2006-01-02"), mp.PricingURL)
	case cost != nil:
		fmt.Fprintf(&b, "  Cost:   $%.4f (reported by provider)", *cost)
	default:
		b.WriteString("  Cost:   unknown (no pricing data for this model)")
	}
	return b.String()
}

// formatNumber adds commas to large numbers
func formatNumber(n int64) string {
	str := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(str, "-")
	str = strings.TrimPrefix(str, "-")
	var b strings.Builder
	for i, c := range str {
		if i > 0 && (len(str)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
