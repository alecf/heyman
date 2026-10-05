package eval

import (
	"github.com/alecf/heyman/internal/llm"
	"github.com/alecf/heyman/internal/pricing"
)

// EstimateCost returns the cost of a call: reported (if non-nil), else from
// the pricing database (cache reads at 0.1x and cache writes at 1.25x the
// input price), 0 for local models, or nil when unknown.
func EstimateCost(spec llm.Spec, reported *float64, in, out, cacheRead, cacheWrite int64) *float64 {
	if reported != nil {
		v := *reported
		return &v
	}
	if pricing.IsFree(spec.Provider) {
		zero := 0.0
		return &zero
	}
	mp := pricing.GetDatabase().GetPricing(spec.String())
	if mp == nil {
		return nil
	}
	v := mp.CalculateCost(in, out) +
		float64(cacheRead)/1e6*mp.InputPerMillion*0.1 +
		float64(cacheWrite)/1e6*mp.InputPerMillion*1.25
	return &v
}
