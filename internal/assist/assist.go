// Package assist turns a natural-language request into a shell command by
// running an LLM tool loop over the local man pages.
package assist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/alecf/heyman/internal/manpage"
)

// ManSource provides man pages. *manpage.Fetcher satisfies it; tests use fakes.
type ManSource interface {
	Fetch(name, section string) (string, error)
	Apropos(keyword string) (string, error)
}

// Request is a single heyman query.
type Request struct {
	// Command is the program the user named (`heyman <command> …`). Its man
	// page is preloaded into the prompt. Empty means `heyman -- …`: the model
	// chooses which pages to read.
	Command  string
	Section  string
	Question string
	Explain  bool
}

// Usage is token usage summed across every step of the tool loop.
type Usage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

// ToolCall records one tool invocation, for --verbose and eval traces.
type ToolCall struct {
	Tool  string `json:"tool"`
	Input string `json:"input"`
	Error string `json:"error,omitempty"`
}

// Result is the outcome of Ask.
type Result struct {
	Command     string     `json:"command"`
	Explanation string     `json:"explanation,omitempty"`
	ManPages    []string   `json:"man_pages,omitempty"`
	ToolCalls   []ToolCall `json:"tool_calls,omitempty"`
	Steps       int        `json:"steps"`
	Usage       Usage      `json:"usage"`
	Model       string     `json:"model"`
	Cached      bool       `json:"cached,omitempty"`
	// CostUSD is set by providers that report cost themselves (claude-code).
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// ErrNoCommand is returned when the model finishes without producing a command.
var ErrNoCommand = errors.New("model did not produce a command")

// Answerer is anything that can answer a Request. *Assistant and the
// claude-code backend both implement it.
type Answerer interface {
	Ask(ctx context.Context, req Request) (*Result, error)
}

// Event reports progress during Ask (e.g. for a spinner).
type Event struct {
	Kind   string // "tool"
	Tool   string
	Detail string
}

// Assistant answers requests with a fantasy language model.
type Assistant struct {
	Model     fantasy.LanguageModel
	ModelName string // "provider/model", recorded in results
	Man       ManSource

	// MaxSteps bounds the tool loop. Default 10.
	MaxSteps int
	// PreloadChars caps how much of the named command's man page goes into
	// the system prompt. Default 48000 (~12k tokens).
	PreloadChars int
	// PageChars caps each `man` tool response. Default 24000.
	PageChars int
	// MaxOutputTokens per model call. Default 8192.
	MaxOutputTokens int64
	// NoTools disables tool calling (for models that don't support it). The
	// named command's man page is still preloaded.
	NoTools bool

	OnEvent func(Event)
}

func (a *Assistant) defaults() {
	if a.MaxSteps == 0 {
		a.MaxSteps = 10
	}
	if a.PreloadChars == 0 {
		a.PreloadChars = 48000
	}
	if a.PageChars == 0 {
		a.PageChars = 24000
	}
	if a.MaxOutputTokens == 0 {
		a.MaxOutputTokens = 8192
	}
}

// Ask runs the tool loop and returns the command.
func (a *Assistant) Ask(ctx context.Context, req Request) (*Result, error) {
	a.defaults()
	if strings.TrimSpace(req.Question) == "" {
		return nil, fmt.Errorf("no question specified")
	}

	run := &run{a: a, req: req, pages: map[string]string{}}

	var preload string
	if req.Command != "" {
		page, err := a.Man.Fetch(req.Command, req.Section)
		if err != nil {
			return nil, err
		}
		run.cachePage(req.Command, req.Section, page)
		run.consulted(req.Command, req.Section)
		preload = page
	}

	res, err := run.generate(ctx, preload, !a.NoTools)
	if err != nil && !a.NoTools && toolsUnsupported(err) {
		// Some local models reject tool definitions outright; fall back to a
		// single-shot prompt with the preloaded page.
		run.reset()
		res, err = run.generate(ctx, preload, false)
	}
	return res, err
}

func toolsUnsupported(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "does not support tools") ||
		strings.Contains(msg, "tools are not supported") ||
		strings.Contains(msg, "tool use is not supported")
}

// run holds per-request state shared with the tool closures.
type run struct {
	a   *Assistant
	req Request

	mu        sync.Mutex
	pages     map[string]string // "name(section)" -> cleaned page
	manPages  []string
	toolCalls []ToolCall
	answer    *answerInput
}

func (r *run) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolCalls = nil
	r.answer = nil
}

func pageKey(name, section string) string {
	if section == "" {
		return name
	}
	return name + "(" + section + ")"
}

func (r *run) cachePage(name, section, page string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pages[pageKey(name, section)] = page
}

func (r *run) consulted(name, section string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := pageKey(name, section)
	for _, p := range r.manPages {
		if p == key {
			return
		}
	}
	r.manPages = append(r.manPages, key)
}

func (r *run) record(call ToolCall) {
	r.mu.Lock()
	r.toolCalls = append(r.toolCalls, call)
	r.mu.Unlock()
	if r.a.OnEvent != nil {
		r.a.OnEvent(Event{Kind: "tool", Tool: call.Tool, Detail: call.Input})
	}
}

func (r *run) generate(ctx context.Context, preload string, withTools bool) (*Result, error) {
	a := r.a
	opts := []fantasy.AgentOption{
		fantasy.WithSystemPrompt(systemPrompt(r.req, preload, a.PreloadChars, withTools)),
		fantasy.WithMaxOutputTokens(a.MaxOutputTokens),
	}
	if withTools {
		opts = append(opts,
			fantasy.WithTools(r.manTool(), r.manSearchTool(), r.whichTool(), r.answerTool()),
			fantasy.WithStopConditions(fantasy.StepCountIs(a.MaxSteps)),
		)
	}
	agent := fantasy.NewAgent(a.Model, opts...)

	out, err := agent.Generate(ctx, fantasy.AgentCall{Prompt: userPrompt(r.req)})
	if err != nil {
		return nil, err
	}

	res := &Result{
		Model: a.ModelName,
		Steps: len(out.Steps),
		Usage: Usage{
			InputTokens:      out.TotalUsage.InputTokens,
			OutputTokens:     out.TotalUsage.OutputTokens,
			CacheReadTokens:  out.TotalUsage.CacheReadTokens,
			CacheWriteTokens: out.TotalUsage.CacheCreationTokens,
		},
	}
	r.mu.Lock()
	res.ManPages = append([]string(nil), r.manPages...)
	res.ToolCalls = append([]ToolCall(nil), r.toolCalls...)
	answer := r.answer
	r.mu.Unlock()

	if answer != nil {
		res.Command = cleanCommand(answer.Command)
		res.Explanation = strings.TrimSpace(answer.Explanation)
	} else {
		// No answer tool call: fall back to parsing the final text.
		res.Command, res.Explanation = ParseText(out.Response.Content.Text())
	}
	if res.Command == "" {
		return res, ErrNoCommand
	}
	return res, nil
}

// --- tools ---

type manInput struct {
	Page    string `json:"page" description:"Man page name, e.g. \"xargs\", \"git-log\" (git subcommands are git-<sub>), \"stat\""`
	Section string `json:"section,omitempty" description:"Optional man section, e.g. \"1\", \"5\", \"8\""`
	Search  string `json:"search,omitempty" description:"Optional case-insensitive text or regex; returns only matching lines with context instead of the page"`
	Offset  int    `json:"offset,omitempty" description:"Character offset to continue reading a long page from (see the header of the previous result)"`
}

func (r *run) manTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool("man",
		"Read a man page from THIS machine. Flags differ between systems (e.g. BSD vs GNU), so check the local page before relying on a flag. Use `search` to jump to the options you care about in long pages.",
		func(ctx context.Context, in manInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			call := ToolCall{Tool: "man", Input: describeMan(in)}
			text, err := r.readMan(in)
			if err != nil {
				call.Error = err.Error()
				r.record(call)
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			r.record(call)
			return fantasy.NewTextResponse(text), nil
		})
}

func describeMan(in manInput) string {
	s := pageKey(in.Page, in.Section)
	if in.Search != "" {
		s += fmt.Sprintf(" search=%q", in.Search)
	}
	if in.Offset > 0 {
		s += fmt.Sprintf(" offset=%d", in.Offset)
	}
	return s
}

func (r *run) readMan(in manInput) (string, error) {
	name := strings.TrimSpace(in.Page)
	section := strings.TrimSpace(in.Section)
	if name == "" {
		return "", fmt.Errorf("page is required")
	}
	key := pageKey(name, section)

	r.mu.Lock()
	page, ok := r.pages[key]
	r.mu.Unlock()
	if !ok {
		var err error
		page, err = r.a.Man.Fetch(name, section)
		if err != nil {
			return "", err
		}
		r.cachePage(name, section, page)
	}
	r.consulted(name, section)

	if in.Search != "" {
		return Grep(page, in.Search, 2, r.a.PageChars), nil
	}
	return Chunk(key, page, in.Offset, r.a.PageChars), nil
}

type manSearchInput struct {
	Keyword string `json:"keyword" description:"A word describing the task, e.g. \"checksum\", \"archive\", \"port\""`
}

func (r *run) manSearchTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool("man_search",
		"Search man page names and one-line descriptions on this machine (like `man -k`). Use it to discover which installed program can do something.",
		func(ctx context.Context, in manSearchInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			call := ToolCall{Tool: "man_search", Input: in.Keyword}
			out, err := r.a.Man.Apropos(in.Keyword)
			if err != nil {
				call.Error = err.Error()
				r.record(call)
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			r.record(call)
			return fantasy.NewTextResponse(limitLines(out, 60)), nil
		})
}

type whichInput struct {
	Program string `json:"program" description:"Program name to look up on PATH"`
}

func (r *run) whichTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool("which",
		"Check whether a program is installed on this machine and where.",
		func(ctx context.Context, in whichInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			call := ToolCall{Tool: "which", Input: in.Program}
			r.record(call)
			if manpage.ValidateName(in.Program) != nil {
				return fantasy.NewTextErrorResponse("invalid program name"), nil
			}
			path, err := exec.LookPath(in.Program)
			if err != nil {
				return fantasy.NewTextResponse(in.Program + ": not installed"), nil
			}
			return fantasy.NewTextResponse(path), nil
		})
}

type answerInput struct {
	Command     string `json:"command" description:"The complete shell command (a pipeline is fine). No markdown, no prompt character."`
	Explanation string `json:"explanation,omitempty" description:"Short explanation of what the command does and why these flags"`
}

func (r *run) answerTool() fantasy.AgentTool {
	return fantasy.NewAgentTool("answer",
		"Give the final command. Call this exactly once, when you are done.",
		func(ctx context.Context, in answerInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(in.Command) == "" {
				return fantasy.NewTextErrorResponse("command must not be empty"), nil
			}
			if r.req.Explain && strings.TrimSpace(in.Explanation) == "" {
				return fantasy.NewTextErrorResponse("the user asked for an explanation: call answer again with an explanation"), nil
			}
			r.mu.Lock()
			r.answer = &in
			r.mu.Unlock()
			raw, _ := json.Marshal(in)
			r.record(ToolCall{Tool: "answer", Input: string(raw)})
			resp := fantasy.NewTextResponse("ok")
			resp.StopTurn = true
			return resp, nil
		})
}

// --- prompts ---

func systemPrompt(req Request, preload string, preloadChars int, withTools bool) string {
	var b strings.Builder
	b.WriteString(`You are heyman, an expert on the Unix command line. The user describes something they want to do in a terminal; you give them a single shell command that does it on their machine.

`)
	fmt.Fprintf(&b, "The user's machine: %s. Shell: %s.\n", machineDescription(), shellName())
	if runtime.GOOS == "darwin" {
		b.WriteString("macOS ships BSD versions of most core utilities (stat, sed, date, find, xargs, du, …), whose flags differ from GNU/Linux. Do not assume GNU options.\n")
	}
	b.WriteString("\nGuidelines:\n")
	b.WriteString("- Answer with one command. Pipelines and `&&` chains are fine; prefer the simplest correct command.\n")
	b.WriteString("- Use only flags that exist on this machine. ")
	if withTools {
		b.WriteString("When you are not certain a flag exists here or what it does, read the man page with the `man` tool (use `search` to find options in long pages). Check every program in a pipeline you are unsure about, not just the first one.\n")
	} else {
		b.WriteString("Rely on the man page below.\n")
	}
	b.WriteString("- If the request names concrete values (files, ports, patterns), use them. Otherwise use descriptive placeholders like <PID> or <file>.\n")
	b.WriteString("- Prefer read-only, non-destructive commands. Don't add sudo unless it is required.\n")
	b.WriteString("- The request comes from the user's command line, so it may be terse or ungrammatical. Interpret it the way a shell user would mean it.\n")
	if withTools {
		b.WriteString("- When you are done, call the `answer` tool exactly once")
		if req.Explain {
			b.WriteString(" with the command and a concise explanation of what it does and why those flags.\n")
		} else {
			b.WriteString(" with the command.\n")
		}
	} else if req.Explain {
		b.WriteString("- Reply with the command alone on the first line, then a blank line, then a concise explanation. No markdown.\n")
	} else {
		b.WriteString("- Reply with ONLY the command. No explanation, no markdown, no code fences.\n")
	}

	if req.Command != "" {
		fmt.Fprintf(&b, "\nThe user asked specifically about `%s`; the command will most likely use it, possibly combined with other programs.\n", req.Command)
		page := preload
		note := ""
		if len(page) > preloadChars {
			page = cutAtLine(page, preloadChars)
			if withTools {
				note = fmt.Sprintf("\n[truncated: page continues past character %d; use the man tool with offset or search to read more]", len(page))
			} else {
				note = "\n[truncated]"
			}
		}
		fmt.Fprintf(&b, "\nHere is its man page from this machine:\n<manpage name=%q>\n%s%s\n</manpage>\n", pageKey(req.Command, req.Section), page, note)
	}
	return b.String()
}

func userPrompt(req Request) string {
	return req.Question
}

var (
	machineOnce sync.Once
	machineDesc string
)

func machineDescription() string {
	machineOnce.Do(func() {
		machineDesc = runtime.GOOS + "/" + runtime.GOARCH
		switch runtime.GOOS {
		case "darwin":
			if v, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
				machineDesc = "macOS " + strings.TrimSpace(string(v)) + " (" + runtime.GOARCH + ")"
			}
		case "linux":
			if data, err := os.ReadFile("/etc/os-release"); err == nil {
				for _, line := range strings.Split(string(data), "\n") {
					if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
						machineDesc = "Linux, " + strings.Trim(v, `"`) + " (" + runtime.GOARCH + ")"
					}
				}
			}
		}
	})
	return machineDesc
}

func shellName() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh[strings.LastIndex(sh, "/")+1:]
	}
	return "sh"
}

// --- text helpers ---

var fence = regexp.MustCompile("(?s)```[a-zA-Z]*\\s*\\n?(.*?)```")

// ParseText extracts a command (and optional explanation) from a free-text
// model reply: the contents of the first code fence if there is one,
// otherwise the first non-empty line.
func ParseText(text string) (command, explanation string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ""
	}
	if m := fence.FindStringSubmatchIndex(text); m != nil {
		command = cleanCommand(text[m[2]:m[3]])
		rest := strings.TrimSpace(text[:m[0]] + "\n" + text[m[1]:])
		return command, rest
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		return cleanCommand(line), strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
	}
	return "", ""
}

// cleanCommand strips formatting models commonly wrap commands in.
func cleanCommand(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimSuffix(strings.TrimPrefix(s, "```"), "```")
		// Drop a language tag line such as "bash".
		if first, rest, ok := strings.Cut(s, "\n"); ok && !strings.Contains(strings.TrimSpace(first), " ") {
			s = rest
		}
	}
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`")
	s = strings.TrimPrefix(s, "$ ")
	return strings.TrimSpace(s)
}

// Chunk returns page[offset:offset+limit] cut at a line boundary, with a
// header telling the model how to continue.
func Chunk(name, page string, offset, limit int) string {
	if offset < 0 || offset >= len(page) {
		offset = 0
	}
	rest := page[offset:]
	body := rest
	if len(rest) > limit {
		body = cutAtLine(rest, limit)
	}
	end := offset + len(body)
	if offset == 0 && end >= len(page) {
		return body
	}
	header := fmt.Sprintf("[man %s: characters %d-%d of %d", name, offset, end, len(page))
	if end < len(page) {
		header += fmt.Sprintf("; call again with offset=%d for more, or use search", end)
	}
	return header + "]\n" + body
}

func cutAtLine(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	if i := strings.LastIndex(cut, "\n"); i > limit/2 {
		cut = cut[:i+1]
	}
	return cut
}

// Grep returns lines of page matching pattern (case-insensitive regex, or a
// literal if the pattern doesn't compile), with context lines around each
// match, capped at limit characters.
func Grep(page, pattern string, context, limit int) string {
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(pattern))
	}
	lines := strings.Split(page, "\n")
	keep := make([]bool, len(lines))
	matches := 0
	for i, line := range lines {
		if re.MatchString(line) {
			matches++
			for j := max(0, i-context); j <= min(len(lines)-1, i+context); j++ {
				keep[j] = true
			}
		}
	}
	if matches == 0 {
		return fmt.Sprintf("no lines match %q", pattern)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[%d matching lines for %q]\n", matches, pattern)
	prev := -2
	for i, k := range keep {
		if !k {
			continue
		}
		if prev >= 0 && i != prev+1 {
			b.WriteString("--\n")
		}
		prev = i
		if b.Len()+len(lines[i]) > limit {
			b.WriteString("[more matches truncated; use a narrower search]\n")
			break
		}
		b.WriteString(lines[i])
		b.WriteString("\n")
	}
	return b.String()
}

func limitLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n[%d more results; use a more specific keyword]", len(lines)-n)
}
