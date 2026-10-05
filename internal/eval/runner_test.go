package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alecf/heyman/internal/assist"
	"github.com/alecf/heyman/internal/llm"
)

type fakeAnswerer struct {
	answers map[string]string // question -> command
	err     error
}

func (f *fakeAnswerer) Ask(_ context.Context, req assist.Request) (*assist.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	res := &assist.Result{
		Command: f.answers[req.Question],
		Steps:   2,
		Usage:   assist.Usage{InputTokens: 1000, OutputTokens: 100},
		ToolCalls: []assist.ToolCall{
			{Tool: "man", Input: "head"},
			{Tool: "answer", Input: "{}"},
		},
	}
	if req.Command != "" {
		res.ManPages = []string{req.Command}
	} else {
		res.ManPages = []string{"head"}
	}
	return res, nil
}

func TestRunAndReport(t *testing.T) {
	s := mustSuite(t)
	var cases []*Case
	for i := range s.Cases {
		cases = append(cases, &s.Cases[i])
	}
	good := &fakeAnswerer{answers: map[string]string{
		"which process is listening on port 8080": "lsof -i :8080",
		"top 10 processes by memory":              "ps aux -m | head -n 11",
		"count lines in all .txt files":           "cat *.txt | wc -l",
	}}
	bad := &fakeAnswerer{answers: map[string]string{
		"which process is listening on port 8080": "netstat -an | grep 8080",
		"top 10 processes by memory":              "ps aux --sort=-%mem | head",
		"count lines in all .txt files":           "wc -l *.txt",
	}}
	broken := &fakeAnswerer{err: errors.New("provider exploded")}
	factory := func(_ context.Context, model string) (assist.Answerer, llm.Spec, error) {
		spec, _ := llm.ParseSpec(model)
		switch model {
		case "anthropic/claude-haiku-4-5":
			return good, spec, nil
		case "ollama/bad:1b":
			return bad, spec, nil
		}
		return broken, spec, nil
	}
	models := []string{"anthropic/claude-haiku-4-5", "ollama/bad:1b", "openai/gpt-x"}
	var out bytes.Buffer
	attempts, stats := Run(context.Background(), cases, Options{
		Models: models, Repeat: 2, Parallel: 3, GOOS: "darwin",
		NewAnswerer: factory, Results: &out,
	})
	if stats.Planned != 18 || stats.Completed != 18 || len(attempts) != 18 {
		t.Fatalf("stats %+v, %d attempts", stats, len(attempts))
	}
	if n := strings.Count(out.String(), "\n"); n != 18 {
		t.Errorf("results.jsonl has %d lines", n)
	}
	var first Attempt
	if err := json.Unmarshal([]byte(strings.SplitN(out.String(), "\n", 2)[0]), &first); err != nil {
		t.Fatal(err)
	}
	// Ordering: model, case, repeat.
	if attempts[0].Model != models[0] || attempts[0].CaseID != "lsof-port" || attempts[1].Repeat != 2 {
		t.Errorf("unexpected order: %+v %+v", attempts[0], attempts[1])
	}

	rep := BuildReport(models, cases, attempts)
	byModel := map[string]*ModelSummary{}
	for _, s := range rep.Summary {
		byModel[s.Model] = s
	}
	g := byModel[models[0]]
	if g.Overall != (Rate{6, 6}) || g.Errors != 0 {
		t.Errorf("good: %+v", g)
	}
	if *g.ByTag["bsd-gnu"] != (Rate{2, 2}) || *g.ByForm["command"] != (Rate{2, 2}) || *g.ByForm["--"] != (Rate{4, 4}) {
		t.Errorf("good tags/forms: %+v %+v", g.ByTag, g.ByForm)
	}
	if g.AvgToolCalls != 1 {
		t.Errorf("avg tool calls %v (answer must not count)", g.AvgToolCalls)
	}
	// lsof case consulted only the preloaded page; the two -- cases read head.
	if int(g.ConsultedExtraPct+0.5) != 67 {
		t.Errorf("consulted extra %v", g.ConsultedExtraPct)
	}
	// haiku: 1000 in * $1/M + 100 out * $5/M = $0.0015 per attempt
	if !g.CostKnown || abs(g.CostTotal-6*0.0015) > 1e-9 {
		t.Errorf("cost %v known=%v", g.CostTotal, g.CostKnown)
	}
	b := byModel["ollama/bad:1b"]
	if b.Overall != (Rate{2, 6}) || b.CostTotal != 0 || !b.CostKnown {
		t.Errorf("bad: %+v", b)
	}
	e := byModel["openai/gpt-x"]
	if e.Errors != 6 || e.Overall.Pass != 0 || e.CostKnown {
		t.Errorf("errors: %+v", e)
	}

	md := rep.Markdown()
	for _, want := range []string{"## Summary", "100% (6/6)", "33% (2/6)", "## Per-case results", "2/2", "0/2", "## Failures", "provider exploded", "got:   netstat -an | grep 8080", "ref:   lsof -nP"} {
		if !strings.Contains(md, want) {
			t.Errorf("report missing %q\n%s", want, md)
		}
	}
}

func TestRunSkipsAndBudget(t *testing.T) {
	c := &Case{ID: "mac", Question: "q", Tags: []string{"macos-only", "easy"}, Reference: []string{"ls"}, Checks: []Check{{Program: "ls"}}}
	c2 := &Case{ID: "x", Question: "q2", Tags: []string{"easy"}, Reference: []string{"ls"}, Checks: []Check{{Program: "ls"}}}
	f := &fakeAnswerer{answers: map[string]string{"q": "ls", "q2": "ls"}}
	factory := func(_ context.Context, model string) (assist.Answerer, llm.Spec, error) {
		spec, _ := llm.ParseSpec(model)
		return f, spec, nil
	}
	attempts, _ := Run(context.Background(), []*Case{c}, Options{Models: []string{"anthropic/claude-haiku-4-5"}, GOOS: "linux", NewAnswerer: factory})
	if len(attempts) != 1 || attempts[0].Skipped == "" {
		t.Fatalf("expected skip: %+v", attempts)
	}
	rep := BuildReport([]string{"anthropic/claude-haiku-4-5"}, []*Case{c}, attempts)
	if rep.Summary[0].Overall.N != 0 || rep.Summary[0].Skipped != 1 {
		t.Errorf("skip counted: %+v", rep.Summary[0])
	}

	// Budget: each attempt costs $0.0015; a $0.004 cap allows two sequential attempts
	// (the third would be estimated to exceed it).
	var cases []*Case
	for range 10 {
		cases = append(cases, c2)
	}
	attempts, stats := Run(context.Background(), cases, Options{Models: []string{"anthropic/claude-haiku-4-5"}, Parallel: 1, MaxCost: 0.004, GOOS: "linux", NewAnswerer: factory})
	if !stats.BudgetHit || len(attempts) != 2 {
		t.Errorf("budget: %d attempts, %+v", len(attempts), stats)
	}
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Case{ID: "x", Question: "q", Tags: []string{"easy"}, Reference: []string{"ls"}, Checks: []Check{{Program: "ls"}}}
	attempts, stats := Run(ctx, []*Case{c, c, c}, Options{Models: []string{"anthropic/claude-haiku-4-5"},
		NewAnswerer: func(context.Context, string) (assist.Answerer, llm.Spec, error) {
			return &fakeAnswerer{}, llm.Spec{}, nil
		}})
	if !stats.Interrupted || len(attempts) != 0 {
		t.Errorf("cancelled run: %d attempts %+v", len(attempts), stats)
	}
}

func TestPercentile(t *testing.T) {
	xs := []float64{5, 1, 4, 2, 3, 6, 7, 8, 9, 10}
	if p := percentile(xs, 50); p != 5 {
		t.Errorf("p50 = %v", p)
	}
	if p := percentile(xs, 95); p != 10 {
		t.Errorf("p95 = %v", p)
	}
	if p := percentile(nil, 50); p != 0 {
		t.Errorf("empty = %v", p)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
