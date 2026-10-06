// Command heyman-eval runs heyman's evaluation suite (evals/cases.yaml)
// against one or more models and reports pass rates, latency and cost.
//
//	go run ./cmd/heyman-eval --models anthropic/claude-haiku-4-5,ollama/qwen3:0.6b --exec
//	go run ./cmd/heyman-eval --self-test --exec
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alecf/heyman/internal/assist"
	"github.com/alecf/heyman/internal/eval"
	"github.com/alecf/heyman/internal/llm"
	"github.com/alecf/heyman/internal/manpage"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		casesPath = flag.String("cases", "evals/cases.yaml", "case file")
		models    = flag.String("models", llm.DefaultModel, "comma-separated provider/model specs")
		filter    = flag.String("filter", "", "regex on case id or tag")
		parallel  = flag.Int("parallel", 4, "concurrent API attempts (ollama models always run one at a time)")
		repeat    = flag.Int("repeat", 1, "run each case K times")
		timeout   = flag.Duration("timeout", 3*time.Minute, "per-attempt timeout for the model call")
		doExec    = flag.Bool("exec", false, "run execution checks in a sandbox")
		judge     = flag.String("judge", "", "LLM judge model spec (default off)")
		outDir    = flag.String("out", "", "results directory (default evals/results/<UTC timestamp>)")
		maxCost   = flag.Float64("max-cost", 0, "stop scheduling attempts before estimated spend exceeds this many USD (0 = no limit)")
		selfTest  = flag.Bool("self-test", false, "grade the references instead of calling models")
		list      = flag.Bool("list", false, "list matching cases and exit")
		aliases   = flag.String("alias", "", "with --export-site: rename models, e.g. ollama/hm-gemma4-12b:32k=ollama/gemma4:12b,…")
		exportTo  = flag.String("export-site", "", "write a site JSON snapshot of the result dirs given as arguments (re-graded with the current cases; no model calls) and exit")
		preload   = flag.Int("preload-chars", 0, "characters of the named man page to preload (0 = heyman default)")
		pageChars = flag.Int("page-chars", 0, "max characters per man() tool result (0 = heyman default)")
		effort    = flag.String("reasoning-effort", "", "reasoning effort for ollama/openai-compat models (none disables thinking)")
		gradeCmd  = flag.String("grade", "", "grade this command against the matching cases (deterministic checks; exec too with --exec) and exit")
	)
	flag.Parse()

	suite, err := eval.LoadFile(*casesPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loading cases:", err)
		return 2
	}
	var re *regexp.Regexp
	if *filter != "" {
		if re, err = regexp.Compile(*filter); err != nil {
			fmt.Fprintln(os.Stderr, "bad --filter:", err)
			return 2
		}
	}
	cases := suite.Filter(re)
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "no cases match")
		return 2
	}

	if *list {
		for _, c := range cases {
			form := "--"
			if c.Command != "" {
				form = c.Command
			}
			ex := ""
			if c.Exec != nil {
				ex = " [exec:" + c.Exec.Compare + "]"
			}
			fmt.Printf("%-30s %-8s %-10s %s%s\n    %s\n", c.ID, c.Difficulty(), form, strings.Join(c.Tags, ","), ex, c.Question)
		}
		fmt.Printf("%d cases\n", len(cases))
		return 0
	}

	if *exportTo != "" {
		am := map[string]string{}
		for _, pair := range splitList(*aliases) {
			if from, to, ok := strings.Cut(pair, "="); ok {
				am[strings.TrimSpace(from)] = strings.TrimSpace(to)
			}
		}
		return exportSite(cases, *exportTo, flag.Args(), am)
	}

	if *gradeCmd != "" {
		var ex *eval.Executor
		if *doExec {
			ex = &eval.Executor{Sandbox: eval.DetectSandbox()}
		}
		failed := 0
		for _, c := range cases {
			g := c.GradeCommand(*gradeCmd, runtime.GOOS)
			status := "PASS"
			if !g.Pass {
				status = "FAIL " + g.FirstFailure
				failed++
			}
			fmt.Printf("%-30s %s\n", c.ID, status)
			if ex != nil && c.Exec != nil {
				r := ex.Run(context.Background(), c, *gradeCmd)
				fmt.Printf("%-30s exec %s %s\n", "", r.Status, r.Reason)
			}
		}
		if failed > 0 {
			return 1
		}
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var executor *eval.Executor
	if *doExec {
		sb := eval.DetectSandbox()
		if sb == "" {
			fmt.Fprintln(os.Stderr, "warning: --exec: no sandbox available (sandbox-exec on macOS, bwrap on Linux); exec checks will be skipped")
		}
		executor = &eval.Executor{Sandbox: sb}
	}

	if *selfTest {
		return runSelfTest(ctx, cases, executor, *parallel)
	}

	modelList := splitList(*models)
	if len(modelList) == 0 {
		fmt.Fprintln(os.Stderr, "no models")
		return 2
	}
	for _, m := range modelList {
		if _, err := llm.ParseSpec(m); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}

	var j *eval.Judge
	if *judge != "" {
		if j, err = eval.NewJudge(ctx, *judge); err != nil {
			fmt.Fprintln(os.Stderr, "judge:", err)
			return 2
		}
	}

	dir := *outDir
	if dir == "" {
		dir = filepath.Join("evals", "results", time.Now().UTC().Format("20060102T150405Z"))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resultsFile, err := os.Create(filepath.Join(dir, "results.jsonl"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resultsFile.Close()
	results := bufio.NewWriter(resultsFile)
	lineWriter := &flushWriter{w: results}

	meta := map[string]any{
		"started":       time.Now().UTC().Format(time.RFC3339),
		"cases":         *casesPath,
		"filter":        *filter,
		"n_cases":       len(cases),
		"models":        modelList,
		"repeat":        *repeat,
		"parallel":      *parallel,
		"timeout":       timeout.String(),
		"exec":          *doExec,
		"judge":         *judge,
		"max_cost":      *maxCost,
		"os":            eval.OSDescription(),
		"preload_chars": *preload,
		"page_chars":    *pageChars,
		"hardware":      eval.DetectHardware(),
		"git_head":      gitHead(),
		"go":            runtime.Version(),
		"command":       strings.Join(os.Args, " "),
	}

	fmt.Fprintf(os.Stderr, "heyman-eval: %d cases × %d models × %d repeats → %s\n", len(cases), len(modelList), *repeat, dir)
	man := manpage.NewFetcher()
	// For local models, record the Ollama version, settings and the peak
	// memory / context each model used, so results can be compared.
	var sampler *eval.OllamaSampler
	for _, m := range modelList {
		if strings.HasPrefix(m, llm.Ollama+"/") {
			meta["ollama_version"] = eval.OllamaVersion()
			if *effort != "" {
				meta["reasoning_effort"] = *effort
			}
			sampler = eval.StartOllamaSampler(ctx, 5*time.Second)
			break
		}
	}
	// Keep the machine awake for the run: sleeping mid-request freezes local
	// models and skews latency. Then measure whether it slept anyway (wall
	// clock vs. the monotonic clock, which stops during sleep).
	if how := keepAwake(); how != "" {
		meta["kept_awake"] = how
	}
	wallStart, monoStart := time.Now().Round(0), time.Now()

	warmups := map[string]eval.Warmup{}
	var warmMu sync.Mutex
	start := time.Now()
	attempts, stats := eval.Run(ctx, cases, eval.Options{
		Models:   modelList,
		Repeat:   *repeat,
		Parallel: *parallel,
		Timeout:  *timeout,
		Executor: executor,
		Judge:    j,
		MaxCost:  *maxCost,
		PrepareLocal: func(ctx context.Context, model string) error {
			w, err := eval.PrepareOllama(ctx, strings.TrimSuffix(llm.OllamaBaseURL(), "/v1"), model)
			if len(w.Unloaded) > 0 {
				fmt.Fprintf(os.Stderr, "unloaded %s before %s\n", strings.Join(w.Unloaded, ", "), model)
			}
			if err == nil {
				fmt.Fprintf(os.Stderr, "warmed up %s in %.1fs\n", model, w.LoadSeconds)
			}
			warmMu.Lock()
			warmups[model] = w
			warmMu.Unlock()
			return err
		},
		NewAnswerer: func(ctx context.Context, model string) (assist.Answerer, llm.Spec, error) {
			return assist.New(ctx, assist.Config{Model: model, Man: man, ReasoningEffort: *effort, PreloadChars: *preload, PageChars: *pageChars})
		},
		Results:  lineWriter,
		Progress: os.Stderr,
	})
	_ = results.Flush()
	if sampler != nil {
		meta["local_models"] = sampler.Stop()
	}
	if len(warmups) > 0 {
		meta["warmup"] = warmups
	}

	meta["elapsed"] = time.Since(start).Round(time.Second).String()
	if slept := time.Now().Round(0).Sub(wallStart) - time.Since(monoStart); slept > 30*time.Second {
		meta["slept_seconds"] = int(slept.Seconds())
		fmt.Fprintf(os.Stderr, "WARNING: the machine slept for about %s during this run; latencies and timeouts are unreliable. Re-run it.\n", slept.Round(time.Second))
	}
	meta["planned"] = stats.Planned
	meta["completed"] = stats.Completed
	meta["interrupted"] = stats.Interrupted
	meta["budget_hit"] = stats.BudgetHit
	meta["spent_usd"] = stats.SpentUSD
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "run.json"), data, 0o644)
	}

	rep := eval.BuildReport(modelList, cases, attempts)
	var hdr []string
	hdr = append(hdr, fmt.Sprintf("OS: %s · cases: %d · repeat: %d · exec: %v · judge: %s · completed %d/%d in %s · est. spend $%.4f",
		eval.OSDescription(), len(cases), *repeat, *doExec, orNone(*judge), stats.Completed, stats.Planned, meta["elapsed"], stats.SpentUSD))
	if stats.Interrupted {
		hdr = append(hdr, "**Interrupted: partial results.**")
	}
	if stats.BudgetHit {
		hdr = append(hdr, fmt.Sprintf("**Stopped early: --max-cost $%.2f reached.**", *maxCost))
	}
	hdr = append(hdr, "Command: `"+reproCommand()+"`")
	rep.Header = strings.Join(hdr, "\n\n")
	md := rep.Markdown()
	fmt.Println(md)
	if err := os.WriteFile(filepath.Join(dir, "summary.md"), []byte(md), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "wrote %s/{results.jsonl,summary.md,run.json}\n", dir)
	if stats.Interrupted {
		return 130
	}
	return 0
}

func runSelfTest(ctx context.Context, cases []*eval.Case, ex *eval.Executor, parallel int) int {
	issues := eval.SelfTest(ctx, cases, runtime.GOOS, ex, manpage.NewFetcher(), parallel)
	errs := 0
	for _, is := range issues {
		if is.Severity == "error" {
			errs++
		}
		fmt.Printf("%-5s %s: %s\n", strings.ToUpper(is.Severity), is.CaseID, is.Message)
	}
	execNote := "exec not checked (add --exec)"
	if ex != nil {
		execNote = "exec checked with " + orNone(ex.Sandbox)
	}
	fmt.Printf("\nself-test: %d cases, %d errors, %d warnings (%s, %s)\n", len(cases), errs, len(issues)-errs, runtime.GOOS, execNote)
	if errs > 0 {
		return 1
	}
	return 0
}

type flushWriter struct{ w *bufio.Writer }

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = f.w.Flush()
	}
	return n, err
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func gitHead() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// reproCommand renders the invocation as a copy-pasteable `go run` line.
func reproCommand() string {
	parts := []string{"go run ./cmd/heyman-eval"}
	for _, a := range os.Args[1:] {
		if strings.ContainsAny(a, " |()$*?'\"^\\&;<>") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

func exportSite(cases []*eval.Case, out string, dirs []string, aliases map[string]string) int {
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "--export-site needs one or more results directories as arguments")
		return 2
	}
	data, err := eval.BuildSiteData(cases, dirs, "darwin", time.Now().UTC().Format("2006-01-02"), aliases)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	raw, err := json.MarshalIndent(data, "", " ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, m := range data.Models {
		fmt.Printf("%-30s %d/%d\n", m.Model, m.Pass, m.N)
	}
	return 0
}

// keepAwake prevents idle and system sleep for the life of this process
// (macOS caffeinate). It returns a description, or "" if unavailable.
// caffeinate is restarted if something else kills it (some keep-awake
// utilities replace other caffeinate processes).
func keepAwake() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	path, err := exec.LookPath("caffeinate")
	if err != nil {
		return ""
	}
	// -w: exit when this process exits, so nothing is left behind.
	args := []string{"-i", "-s", "-m", "-w", strconv.Itoa(os.Getpid())}
	first := exec.Command(path, args...)
	if err := first.Start(); err != nil {
		return ""
	}
	go func() {
		cmd := first
		for {
			_ = cmd.Wait()
			time.Sleep(time.Second)
			cmd = exec.Command(path, args...)
			if cmd.Start() != nil {
				return
			}
		}
	}()
	return "caffeinate -i -s -m"
}
