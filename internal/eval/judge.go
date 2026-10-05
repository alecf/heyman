package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/alecf/heyman/internal/llm"
)

// Verdict is an LLM judge's opinion of one candidate.
type Verdict struct {
	Verdict string   `json:"verdict,omitempty"` // "correct" | "incorrect"
	Reason  string   `json:"reason,omitempty"`
	Error   string   `json:"error,omitempty"`
	Model   string   `json:"model"`
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// Correct reports whether the judge accepted the candidate.
func (v *Verdict) Correct() bool { return v != nil && v.Verdict == "correct" }

// Judge grades candidates with an LLM.
type Judge struct {
	Spec  llm.Spec
	model fantasy.LanguageModel // nil for claude-code
}

// NewJudge builds a judge for a "provider/model" spec.
func NewJudge(ctx context.Context, model string) (*Judge, error) {
	spec, err := llm.ParseSpec(model)
	if err != nil {
		return nil, err
	}
	j := &Judge{Spec: spec}
	if spec.Provider == llm.ClaudeCode {
		if _, err := exec.LookPath("claude"); err != nil {
			return nil, fmt.Errorf("judge %s needs the claude CLI: %w", model, err)
		}
		return j, nil
	}
	j.model, err = llm.NewLanguageModel(ctx, spec, llm.Options{})
	if err != nil {
		return nil, err
	}
	return j, nil
}

const judgeSystem = `You are a strict, expert grader of shell commands. A user asked a command-line assistant for a single shell command. You decide whether the assistant's command would actually accomplish the user's request when run on the user's machine.

Rules:
- Judge for the stated operating system. Flag validity matters: a flag that does not exist (or means something else) in this OS's version of the tool makes the command incorrect (e.g. GNU-only flags on macOS's BSD utilities).
- Harmless differences are fine: different but valid flags, quoting style, extra safe options, a slightly different output format that still answers the request, using a different tool that works.
- Placeholders like <file> are acceptable only where the request did not give a concrete value.
- A command that is destructive beyond what was asked, or that only partially does the task, is incorrect.
- The reference answers are known-good examples, not the only correct answers.

Reply with ONLY a JSON object, no markdown: {"verdict":"correct"|"incorrect","reason":"<one or two sentences>"}`

var (
	osOnce sync.Once
	osDesc string
)

// OSDescription describes this machine for the judge prompt.
func OSDescription() string {
	osOnce.Do(func() {
		osDesc = runtime.GOOS + "/" + runtime.GOARCH
		if runtime.GOOS == "darwin" {
			if v, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
				osDesc = "macOS " + strings.TrimSpace(string(v)) + " (darwin/" + runtime.GOARCH + ", BSD userland)"
			}
		}
	})
	return osDesc
}

func judgePrompt(c *Case, candidate string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Operating system: %s\n", OSDescription())
	if c.Command != "" {
		fmt.Fprintf(&b, "The user invoked the assistant about the `%s` command.\n", c.Command)
	}
	fmt.Fprintf(&b, "User request: %s\n\nReference answers (known good on this OS):\n", c.Question)
	for _, r := range c.ReferencesFor(runtime.GOOS) {
		fmt.Fprintf(&b, "- %s\n", r)
	}
	fmt.Fprintf(&b, "\nCandidate command to grade:\n%s\n", candidate)
	return b.String()
}

// Grade asks the judge about one candidate. Errors are returned in the Verdict.
func (j *Judge) Grade(ctx context.Context, c *Case, candidate string) *Verdict {
	v := &Verdict{Model: j.Spec.String()}
	prompt := judgePrompt(c, candidate)
	var text string
	if j.model == nil {
		out, cost, err := runClaudeJudge(ctx, j.Spec.Model, prompt)
		v.CostUSD = cost
		if err != nil {
			v.Error = err.Error()
			return v
		}
		text = out
	} else {
		maxTok := int64(1024)
		resp, err := j.model.Generate(ctx, fantasy.Call{
			Prompt:          fantasy.Prompt{fantasy.NewSystemMessage(judgeSystem), fantasy.NewUserMessage(prompt)},
			MaxOutputTokens: &maxTok,
		})
		if err != nil {
			v.Error = err.Error()
			return v
		}
		v.CostUSD = EstimateCost(j.Spec, nil, resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheReadTokens, resp.Usage.CacheCreationTokens)
		text = resp.Content.Text()
	}
	verdict, reason, err := ParseVerdict(text)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Verdict, v.Reason = verdict, reason
	return v
}

var verdictRe = regexp.MustCompile(`(?i)"?verdict"?\s*[:=]\s*"?(correct|incorrect)\b`)

// ParseVerdict extracts {"verdict","reason"} from a judge reply, tolerating
// code fences and surrounding prose.
func ParseVerdict(text string) (verdict, reason string, err error) {
	t := strings.TrimSpace(text)
	if i, k := strings.Index(t, "{"), strings.LastIndex(t, "}"); i >= 0 && k > i {
		var obj struct {
			Verdict string `json:"verdict"`
			Reason  string `json:"reason"`
		}
		if json.Unmarshal([]byte(t[i:k+1]), &obj) == nil {
			v := strings.ToLower(strings.TrimSpace(obj.Verdict))
			if v == "correct" || v == "incorrect" {
				return v, strings.TrimSpace(obj.Reason), nil
			}
		}
	}
	if m := verdictRe.FindStringSubmatch(t); m != nil {
		return strings.ToLower(m[1]), t, nil
	}
	return "", "", fmt.Errorf("unparseable judge reply: %.200q", t)
}

// runClaudeJudge runs `claude -p` with no tools in an empty temp dir.
func runClaudeJudge(ctx context.Context, model, prompt string) (string, *float64, error) {
	dir, err := os.MkdirTemp("", "heyman-judge-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(dir)
	args := []string{"-p", "--output-format", "json", "--system-prompt", judgeSystem, "--tools", "",
		"--setting-sources", "", "--strict-mcp-config", "--no-session-persistence", "--disable-slash-commands"}
	if model != "" {
		args = append(args, "--model", model)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("claude judge: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var out struct {
		Result       string  `json:"result"`
		IsError      bool    `json:"is_error"`
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return "", nil, fmt.Errorf("parsing claude judge output: %w", err)
	}
	cost := out.TotalCostUSD
	if out.IsError {
		return "", &cost, fmt.Errorf("claude judge error: %s", out.Result)
	}
	return out.Result, &cost, nil
}
