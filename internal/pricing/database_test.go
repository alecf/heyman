package pricing

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestGetPricing(t *testing.T) {
	db := GetDatabase()
	tests := map[string]string{
		"anthropic/claude-haiku-4-5":           "claude-haiku-4-5",
		"anthropic/claude-haiku-4-5-20251001":  "claude-haiku-4-5",
		"anthropic/claude-sonnet-5-5-20260101": "claude-sonnet-5-5",
		"anthropic/claude-sonnet-5-20260101":   "claude-sonnet-5",
		"anthropic/claude-opus-5-5-20260101":   "claude-opus-5-5",
		"anthropic/claude-opus-5-20260101":     "claude-opus-5",
		"anthropic/claude-opus-5":              "claude-opus-5",
		"anthropic/claude-opus-55":             "",
		"ollama/qwen3:4b":                      "",
		"claude-code/haiku":                    "",
	}
	// Map iteration order is random; repeat to catch nondeterminism.
	for i := 0; i < 50; i++ {
		for spec, want := range tests {
			mp := db.GetPricing(spec)
			got := ""
			if mp != nil {
				got = mp.Model
			}
			if got != want {
				t.Fatalf("GetPricing(%q) = %q, want %q", spec, got, want)
			}
		}
	}
}

func TestCalculateCost(t *testing.T) {
	mp := &ModelPricing{InputPerMillion: 1, OutputPerMillion: 5}
	if got := mp.CalculateCost(1_000_000, 200_000); math.Abs(got-2) > 1e-9 {
		t.Errorf("cost = %v", got)
	}
}

func TestFormatNumber(t *testing.T) {
	tests := map[int64]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 123456: "123,456", 1234567: "1,234,567", -1234: "-1,234", -999: "-999"}
	for n, want := range tests {
		if got := formatNumber(n); got != want {
			t.Errorf("formatNumber(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestFormatTokenUsage(t *testing.T) {
	c := 0.0123
	mp := &ModelPricing{PricingURL: "https://x"}
	when := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		cost *float64
		free bool
		mp   *ModelPricing
		want string
	}{
		{nil, true, nil, "free (local model)"},
		{&c, false, mp, "$0.0123 (estimated from 2026-01-02"},
		{&c, false, nil, "$0.0123 (reported by provider)"},
		{nil, false, nil, "unknown"},
	}
	for _, tt := range tests {
		got := FormatTokenUsage(1234, 56, tt.cost, tt.free, tt.mp, when)
		if !strings.Contains(got, tt.want) || !strings.Contains(got, "1,290 tokens") {
			t.Errorf("FormatTokenUsage = %q, want %q", got, tt.want)
		}
	}
}
