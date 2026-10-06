package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/alecf/heyman/internal/assist"
	"github.com/alecf/heyman/internal/llm"
)

// Attempt is one (model, case, repeat) result. One JSON line in results.jsonl.
type Attempt struct {
	Model      string   `json:"model"`
	CaseID     string   `json:"case_id"`
	Repeat     int      `json:"repeat"`
	Form       string   `json:"form"`
	Difficulty string   `json:"difficulty"`
	Tags       []string `json:"tags"`

	Command string `json:"command"`
	Error   string `json:"error,omitempty"`
	// Skipped is set (with a reason) when the case was not run at all.
	Skipped string `json:"skipped,omitempty"`

	Pass  bool        `json:"pass"`
	Grade Grade       `json:"grade"`
	Exec  *ExecResult `json:"exec,omitempty"`
	Judge *Verdict    `json:"judge,omitempty"`

	LatencyMS int64             `json:"latency_ms"`
	Steps     int               `json:"steps"`
	ToolCalls []assist.ToolCall `json:"tool_calls,omitempty"`
	ManPages  []string          `json:"man_pages,omitempty"`
	// ConsultedExtra: read a man page beyond the preloaded one.
	ConsultedExtra bool         `json:"consulted_extra_man_page"`
	Usage          assist.Usage `json:"usage"`
	CostUSD        *float64     `json:"cost_usd"`

	// Model's final reply when no command could be extracted.
	RawText      string `json:"raw_text,omitempty"`
	RawReasoning string `json:"raw_reasoning,omitempty"`
}

// Scored reports whether the attempt counts toward pass rates.
func (a *Attempt) Scored() bool { return a.Skipped == "" }

// NonAnswerToolCalls counts tool calls other than the final `answer`.
func (a *Attempt) NonAnswerToolCalls() int {
	n := 0
	for _, tc := range a.ToolCalls {
		if tc.Tool != "answer" {
			n++
		}
	}
	return n
}

// AnswererFactory builds a fresh Answerer for a model spec.
type AnswererFactory func(ctx context.Context, model string) (assist.Answerer, llm.Spec, error)

// Options configure Run.
type Options struct {
	Models   []string
	Repeat   int
	Parallel int           // concurrent API attempts; local (ollama) models always run 1 at a time
	Timeout  time.Duration // per attempt (model call only)
	Executor *Executor     // nil disables exec checks
	Judge    *Judge        // nil disables the judge
	MaxCost  float64       // 0 = unlimited
	GOOS     string

	// PrepareLocal, if set, runs before the first attempt of each local
	// (ollama) model: e.g. unload other models and warm this one up, so
	// load time isn't counted as answer latency.
	PrepareLocal func(ctx context.Context, model string) error

	NewAnswerer AnswererFactory
	Results     io.Writer // JSON lines, written as attempts finish
	Progress    io.Writer // human progress lines (may be nil)
}

// RunStats summarises a run.
type RunStats struct {
	Planned     int
	Completed   int
	Interrupted bool
	BudgetHit   bool
	SpentUSD    float64
}

type job struct {
	model  string
	c      *Case
	repeat int
}

// SkipReason returns why a case can't run here ("" if it can).
func SkipReason(c *Case, goos string) string {
	if c.HasTag("macos-only") && goos != "darwin" {
		return "macos-only"
	}
	if c.HasTag("needs-jq") {
		if _, err := exec.LookPath("jq"); err != nil {
			return "needs-jq (jq not installed)"
		}
	}
	return ""
}

// Run executes every (model, case, repeat) and returns the attempts in a
// stable order. Cancelling ctx stops scheduling; attempts cut short by the
// cancellation are dropped (not counted as failures).
func Run(ctx context.Context, cases []*Case, opts Options) ([]Attempt, RunStats) {
	if opts.Repeat < 1 {
		opts.Repeat = 1
	}
	if opts.Parallel < 1 {
		opts.Parallel = 4
	}
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}

	r := &runner{opts: opts, perModel: map[string]*modelCost{}}
	// Local models share one machine: run them one model at a time (all of
	// a model's attempts before the next model) so Ollama never swaps models
	// between cases. Remote attempts interleave freely.
	var local [][]job
	var remote []job
	for _, m := range opts.Models {
		isLocal := false
		if spec, err := llm.ParseSpec(m); err == nil && spec.Provider == llm.Ollama {
			isLocal = true
		}
		var mine []job
		for rep := 1; rep <= opts.Repeat; rep++ {
			for _, c := range cases {
				mine = append(mine, job{model: m, c: c, repeat: rep})
			}
		}
		if isLocal {
			local = append(local, mine)
		} else {
			remote = append(remote, mine...)
		}
		r.stats.Planned += len(mine)
	}
	// Interleave remote models case by case, as before.
	sort.SliceStable(remote, func(i, k int) bool {
		if remote[i].repeat != remote[k].repeat {
			return remote[i].repeat < remote[k].repeat
		}
		return caseIndex(cases, remote[i].c) < caseIndex(cases, remote[k].c)
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for _, jobs := range local {
			if ctx.Err() != nil {
				return
			}
			if opts.PrepareLocal != nil {
				if err := opts.PrepareLocal(ctx, jobs[0].model); err != nil && opts.Progress != nil {
					fmt.Fprintf(opts.Progress, "warning: preparing %s: %v\n", jobs[0].model, err)
				}
			}
			r.lane(ctx, jobs, 1)
		}
	}()
	go func() { defer wg.Done(); r.lane(ctx, remote, opts.Parallel) }()
	wg.Wait()

	r.stats.Interrupted = ctx.Err() != nil
	// Stable order: model order, then case order, then repeat.
	idx := map[string]int{}
	for i, c := range cases {
		idx[c.ID] = i
	}
	midx := map[string]int{}
	for i, m := range opts.Models {
		midx[m] = i
	}
	sortAttempts(r.attempts, midx, idx)
	return r.attempts, r.stats
}

type modelCost struct {
	n       int
	total   float64
	running int
}

type runner struct {
	opts Options

	mu       sync.Mutex
	attempts []Attempt
	stats    RunStats
	perModel map[string]*modelCost
	judgeN   int
	judgeSum float64
}

func caseIndex(cases []*Case, c *Case) int {
	for i, x := range cases {
		if x == c {
			return i
		}
	}
	return len(cases)
}

// lane runs jobs with at most n in flight.
func (r *runner) lane(ctx context.Context, jobs []job, n int) {
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, j := range jobs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil || !r.reserve(j.model) {
			if ctx.Err() == nil {
				<-sem
			}
			break
		}
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			a := r.attempt(ctx, j)
			r.finish(ctx, j, a)
		}(j)
	}
	wg.Wait()
}

// reserve checks the budget before starting an attempt for model, using the
// average cost of that model's finished attempts (plus the judge) as the
// estimate for this one and every attempt still in flight.
func (r *runner) reserve(model string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	mc := r.perModel[model]
	if mc == nil {
		mc = &modelCost{}
		r.perModel[model] = mc
	}
	if r.opts.MaxCost > 0 {
		committed := r.stats.SpentUSD
		for _, m := range r.perModel {
			committed += float64(m.running) * m.avg()
		}
		est := mc.avg()
		if r.judgeN > 0 {
			est += r.judgeSum / float64(r.judgeN)
		}
		if committed+est > r.opts.MaxCost {
			r.stats.BudgetHit = true
			return false
		}
	}
	mc.running++
	return true
}

func (m *modelCost) avg() float64 {
	if m.n == 0 {
		return 0
	}
	return m.total / float64(m.n)
}

func (r *runner) finish(ctx context.Context, j job, a Attempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mc := r.perModel[j.model]
	mc.running--
	if ctx.Err() != nil && a.Error != "" {
		return // cut short by Ctrl-C: not a model failure
	}
	if a.CostUSD != nil {
		mc.n++
		mc.total += *a.CostUSD
		r.stats.SpentUSD += *a.CostUSD
	}
	if a.Judge != nil && a.Judge.CostUSD != nil {
		r.judgeN++
		r.judgeSum += *a.Judge.CostUSD
		r.stats.SpentUSD += *a.Judge.CostUSD
	}
	r.attempts = append(r.attempts, a)
	r.stats.Completed++
	if r.opts.Results != nil {
		line, _ := json.Marshal(a)
		_, _ = r.opts.Results.Write(append(line, '\n'))
	}
	if r.opts.Progress != nil {
		mark := "✓"
		switch {
		case a.Skipped != "":
			mark = "-"
		case a.Error != "":
			mark = "E"
		case !a.Pass:
			mark = "✗"
		}
		cost := "?"
		if a.CostUSD != nil {
			cost = fmt.Sprintf("$%.4f", *a.CostUSD)
		}
		extra := ""
		if a.Judge != nil && a.Judge.Verdict != "" && a.Judge.Correct() != a.Pass {
			extra = " [judge: " + a.Judge.Verdict + "]"
		}
		fmt.Fprintf(r.opts.Progress, "[%d/%d] %s %-32s %-28s %5.1fs %s (total $%.3f)%s\n",
			r.stats.Completed, r.stats.Planned, mark, a.Model, a.CaseID, float64(a.LatencyMS)/1000, cost, r.stats.SpentUSD, extra)
	}
}

func (r *runner) attempt(ctx context.Context, j job) Attempt {
	c := j.c
	a := Attempt{
		Model: j.model, CaseID: c.ID, Repeat: j.repeat,
		Form: c.Form(), Difficulty: c.Difficulty(), Tags: c.Tags,
	}
	if reason := SkipReason(c, r.opts.GOOS); reason != "" {
		a.Skipped = reason
		return a
	}

	ans, spec, err := r.opts.NewAnswerer(ctx, j.model)
	if err != nil {
		a.Error = err.Error()
		a.Grade = Grade{FirstFailure: "error: " + err.Error()}
		return a
	}
	actx := ctx
	if r.opts.Timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, r.opts.Timeout)
		defer cancel()
	}
	start := time.Now()
	res, err := ans.Ask(actx, assist.Request{Command: c.Command, Section: c.Section, Question: c.Question})
	a.LatencyMS = time.Since(start).Milliseconds()
	if res != nil {
		a.Command = res.Command
		a.Steps = res.Steps
		a.ToolCalls = res.ToolCalls
		a.ManPages = res.ManPages
		a.Usage = res.Usage
		a.RawText, a.RawReasoning = res.RawText, res.RawReasoning
		for _, p := range res.ManPages {
			if c.Command == "" || (p != c.Command && p != pageKey(c.Command, c.Section)) {
				a.ConsultedExtra = true
			}
		}
		a.CostUSD = EstimateCost(spec, res.CostUSD, res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.CacheReadTokens, res.Usage.CacheWriteTokens)
	} else if spec.Provider == llm.Ollama {
		zero := 0.0
		a.CostUSD = &zero
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("timed out after %s: %w", r.opts.Timeout, err)
		}
		a.Error = err.Error()
	}

	a.Grade = c.GradeCommand(a.Command, r.opts.GOOS)
	if a.Error != "" && a.Command == "" {
		a.Grade.FirstFailure = "error: " + a.Error
	}
	a.Pass = a.Grade.Pass && a.Error == ""

	if r.opts.Executor != nil && c.Exec != nil && a.Command != "" && ctx.Err() == nil {
		ex := r.opts.Executor.Run(ctx, c, a.Command)
		a.Exec = &ex
		if ex.Status == ExecFail {
			if a.Pass {
				a.Grade.FirstFailure = "exec: " + ex.Reason
			}
			a.Pass = false
		}
	}
	if r.opts.Judge != nil && a.Command != "" && ctx.Err() == nil {
		a.Judge = r.opts.Judge.Grade(ctx, c, Normalize(a.Command))
	}
	return a
}

func pageKey(name, section string) string {
	if section == "" {
		return name
	}
	return name + "(" + section + ")"
}

func sortAttempts(as []Attempt, models, cases map[string]int) {
	less := func(x, y *Attempt) bool {
		if models[x.Model] != models[y.Model] {
			return models[x.Model] < models[y.Model]
		}
		if cases[x.CaseID] != cases[y.CaseID] {
			return cases[x.CaseID] < cases[y.CaseID]
		}
		return x.Repeat < y.Repeat
	}
	sort.SliceStable(as, func(i, j int) bool { return less(&as[i], &as[j]) })
}
