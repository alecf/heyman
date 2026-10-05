package output

import (
	"encoding/json"
	"testing"

	"github.com/alecf/heyman/internal/assist"
)

func TestFormatJSON(t *testing.T) {
	res := &assist.Result{Command: "ls -la", Explanation: "lists", Model: "a/b", Steps: 2, Cached: true,
		Usage: assist.Usage{InputTokens: 10, OutputTokens: 5}, ManPages: []string{"ls"}, Notes: []string{"n"}}
	cost := 0.5
	for _, explain := range []bool{false, true} {
		s, err := FormatJSON(res, explain, &cost)
		if err != nil {
			t.Fatal(err)
		}
		var out JSONOutput
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			t.Fatal(err)
		}
		if out.Command != "ls -la" || out.Metadata.TokensInput != 10 || !out.Metadata.Cached || *out.Metadata.Cost != 0.5 || len(out.Metadata.Notes) != 1 {
			t.Errorf("out = %+v", out)
		}
		if (out.Explanation != "") != explain {
			t.Errorf("explain=%v explanation=%q", explain, out.Explanation)
		}
	}
	s, _ := FormatJSON(res, false, nil)
	var raw map[string]map[string]any
	_ = json.Unmarshal([]byte(s), &raw)
	if _, ok := raw["metadata"]["cost"]; ok {
		t.Error("unknown cost should be omitted")
	}
}
