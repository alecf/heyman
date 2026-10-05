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
	"unicode/utf8"

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
	// Notes are things the user may want to know about how the answer was
	// produced (e.g. the named command had no man page, or tool calling was
	// unsupported and heyman fell back to a single prompt).
	Notes []string `json:"notes,omitempty"`
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

// Event reports progress during Ask (e.g. for a spinner). Events are
// delivered one at a time, even when tools run in parallel.
type Event struct {
	Kind   string // "tool" or "note"
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
	eventMu sync.Mutex
}

func (a *Assistant) emit(e Event) {
	if a.OnEvent == nil {
		return
	}
	a.eventMu.Lock()
	defer a.eventMu.Unlock()
	a.OnEvent(e)
}

func (a *Assistant) defaults() {
	if a.Man == nil {
		a.Man = manpage.NewFetcher()
	}
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

// Ask runs the tool loop and returns the command. On ErrNoCommand the
// partial Result (usage, tool calls) is returned too.
func (a *Assistant) Ask(ctx context.Context, req Request) (*Result, error) {
	a.defaults()
	if strings.TrimSpace(req.Question) == "" {
		return nil, fmt.Errorf("no question specified")
	}

	req, preload, missing, err := preparePreload(a.Man, req)
	if err != nil {
		return nil, err
	}
	run := &run{a: a, req: req, missing: missing, pages: map[string]string{}}
	if missing != "" {
		run.note(missingNote(missing))
	}
	if req.Command != "" {
		run.cachePage(req.Command, req.Section, preload)
		run.consulted(req.Command, req.Section)
	}

	res, err := run.generate(ctx, preload, !a.NoTools)
	if err != nil && res == nil && !a.NoTools && toolsUnsupported(err) {
		// Some local models reject tool definitions outright; fall back to a
		// single-shot prompt with the preloaded page.
		run.reset()
		run.note("this model does not support tool calling; answered without reading man pages")
		res, err = run.generate(ctx, preload, false)
	}
	return res, err
}

// preparePreload fetches the named command's man page. If the command has no
// man page (it may not be installed, or the first word was just part of the
// request, as in `heyman how do I …`), the request is turned into a free-form
// one: the command word is folded back into the question and missing is set.
func preparePreload(man ManSource, req Request) (out Request, preload, missing string, err error) {
	if req.Command == "" {
		return req, "", "", nil
	}
	page, err := man.Fetch(req.Command, req.Section)
	if err == nil {
		return req, page, "", nil
	}
	if req.Section != "" || !errors.Is(err, manpage.ErrNotFound) {
		// An explicit section means the user is sure it's a man page.
		return req, "", "", err
	}
	missing = req.Command
	req.Question = strings.TrimSpace(req.Command + " " + req.Question)
	req.Command = ""
	return req, "", missing, nil
}

func missingNote(cmd string) string {
	return fmt.Sprintf("no man page for %q on this machine; treated the whole request as free-form", cmd)
}

func toolsUnsupported(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"does not support tools",        // ollama
		"tools are not supported",       //
		"tool use is not supported",     //
		"support tool use",              // openrouter: "No endpoints found that support tool use"
		"does not support function",     // various openai-compatible servers
		"function calling is not",       //
		"tools param requires --jinja",  // llama.cpp server
		"\"auto\" tool choice requires", // vLLM without --enable-auto-tool-choice
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// run holds per-request state shared with the tool closures.
type run struct {
	a       *Assistant
	req     Request
	missing string // command named by the user that has no man page

	mu        sync.Mutex
	pages     map[string]string // "name(section)" -> cleaned page
	manPages  []string
	toolCalls []ToolCall
	notes     []string
	answer    *answerInput
	// rejected is the last answer refused for lacking an explanation; used
	// if the model never retries.
	rejected *answerInput
}

func (r *run) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolCalls = nil
	r.answer = nil
	r.rejected = nil
}

func (r *run) note(msg string) {
	r.mu.Lock()
	r.notes = append(r.notes, msg)
	r.mu.Unlock()
	r.a.emit(Event{Kind: "note", Detail: msg})
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
	r.a.emit(Event{Kind: "tool", Tool: call.Tool, Detail: call.Input})
}

// finalStepNote is appended to the system prompt on the last allowed step,
// when only the answer tool is offered.
const finalStepNote = "\n\nYou have run out of tool calls. Call the `answer` tool now with your best command."

func (r *run) generate(ctx context.Context, preload string, withTools bool) (*Result, error) {
	a := r.a
	system := systemPrompt(r.req, preload, a.PreloadChars, withTools, r.missing)
	opts := []fantasy.AgentOption{
		fantasy.WithSystemPrompt(system),
		fantasy.WithMaxOutputTokens(a.MaxOutputTokens),
	}
	if withTools {
		final := system + finalStepNote
		opts = append(opts,
			fantasy.WithTools(r.manTool(), r.manSearchTool(), r.whichTool(), r.answerTool()),
			fantasy.WithStopConditions(fantasy.StepCountIs(a.MaxSteps)),
			// On the last step, offer only `answer` so the loop ends with a
			// command instead of one more man page read.
			fantasy.WithPrepareStep(func(ctx context.Context, o fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
				if o.StepNumber < a.MaxSteps-1 {
					return ctx, fantasy.PrepareStepResult{}, nil
				}
				return ctx, fantasy.PrepareStepResult{ActiveTools: []string{"answer"}, System: &final}, nil
			}),
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
	res.Notes = append([]string(nil), r.notes...)
	answer, rejected := r.answer, r.rejected
	r.mu.Unlock()

	switch {
	case answer != nil:
		res.Command = cleanCommand(answer.Command)
		res.Explanation = strings.TrimSpace(answer.Explanation)
	default:
		// No accepted answer call: parse the final text (models that reply
		// in prose, or the no-tools fallback).
		res.Command, res.Explanation = ParseText(out.Response.Content.Text())
		if res.Command == "" && rejected != nil {
			// The model answered without the requested explanation and
			// never retried; a command without explanation beats nothing.
			res.Command = cleanCommand(rejected.Command)
		}
	}
	if !r.req.Explain && !withTools {
		res.Explanation = ""
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
			return fantasy.NewTextResponse(cutAtLine(limitLines(out, 60), r.a.PageChars)), nil
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
				r.mu.Lock()
				r.rejected = &in
				r.mu.Unlock()
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

func systemPrompt(req Request, preload string, preloadChars int, withTools bool, missing string) string {
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
	} else if req.Command != "" {
		b.WriteString("Rely on the man page below.\n")
	} else {
		b.WriteString("If you are unsure whether a flag exists on this system, prefer the portable (POSIX) form.\n")
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

	if missing != "" {
		fmt.Fprintf(&b, "\nThe request starts with `%s`, which has no man page on this machine: it may not be installed, or it may just be the first word of the request.\n", missing)
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
// otherwise the first line that isn't an introduction ("Here is the
// command:"). Replies that read as prose or a refusal ("I can't help with
// that.") yield no command.
func ParseText(text string) (command, explanation string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ""
	}
	if m := fence.FindStringSubmatchIndex(text); m != nil {
		command = cleanCommand(text[m[2]:m[3]])
		rest := strings.TrimSpace(strings.TrimSpace(text[:m[0]]) + "\n" + strings.TrimSpace(text[m[1]:]))
		if command == "" {
			return "", ""
		}
		return command, rest
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasSuffix(line, ":") && strings.Contains(line, " ") {
			continue // "Here's the command:" — the command follows
		}
		if looksLikeProse(line) {
			return "", ""
		}
		return cleanCommand(line), strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
	}
	return "", ""
}

// proseStarts are first words of English sentences that are not commands.
var proseStarts = map[string]bool{
	"i": true, "i'm": true, "i'll": true, "i've": true, "i'd": true, "im": true,
	"sorry": true, "unfortunately": true, "apologies": true, "however": true,
	"here": true, "here's": true, "heres": true, "the": true, "this": true, "that": true, "that's": true,
	"there": true, "there's": true, "it": true, "it's": true, "you": true, "your": true,
	"sure": true, "certainly": true, "okay": true, "based": true, "note": true,
	"to": true, "we": true, "let's": true, "a": true, "an": true, "my": true,
	"unable": true, "cannot": true, "can't": true, "no": true,
}

// looksLikeProse reports whether line reads as an English sentence rather
// than a shell command.
func looksLikeProse(line string) bool {
	line = strings.TrimSpace(strings.Trim(strings.TrimSpace(line), "*_`\"'"))
	if line == "" {
		return true
	}
	fields := strings.Fields(line)
	first := strings.ToLower(strings.TrimRight(fields[0], ",.:;!?"))
	first = strings.ReplaceAll(first, "’", "'")
	if proseStarts[first] {
		return true
	}
	if len(fields) >= 2 {
		second := strings.ToLower(fields[1])
		// "Let me…", "As an AI…" (let and as are also real commands).
		if (first == "let" && second == "me") || (first == "as" && (second == "an" || second == "a")) {
			return true
		}
	}
	return false
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
	// Strip inline-code backticks wrapping the whole command, but keep
	// command substitution such as "echo `date`".
	for len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' && strings.Count(s[1:len(s)-1], "`")%2 == 0 {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	s = strings.TrimPrefix(s, "$ ")
	return strings.TrimSpace(s)
}

// Chunk returns page[offset:offset+limit] cut at a line boundary, with a
// header telling the model how to continue.
func Chunk(name, page string, offset, limit int) string {
	if offset < 0 {
		offset = 0
	}
	if offset > 0 && offset >= len(page) {
		return fmt.Sprintf("[man %s: offset %d is past the end of the page (%d characters)]", name, offset, len(page))
	}
	// Don't start in the middle of a UTF-8 sequence.
	for offset > 0 && offset < len(page) && !utf8.RuneStart(page[offset]) {
		offset--
	}
	rest := page[offset:]
	body := cutAtLine(rest, limit)
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

// cutAtLine returns a prefix of s of at most limit bytes, ending at a line
// break if there's one in the second half, and never splitting a UTF-8
// sequence.
func cutAtLine(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit <= 0 {
		return ""
	}
	cut := s[:limit]
	if i := strings.LastIndex(cut, "\n"); i > limit/2 {
		return cut[:i+1]
	}
	for len(cut) > 0 && !utf8.RuneStart(s[len(cut)]) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// maxPattern bounds the regex a model may send to Grep. Go's regexp is
// linear-time (RE2), so there is no catastrophic backtracking, but a huge
// pattern is still expensive to compile.
const maxPattern = 200

// Grep returns lines of page matching pattern (case-insensitive regex, or a
// literal if the pattern doesn't compile), with context lines around each
// match, capped at limit characters.
func Grep(page, pattern string, context, limit int) string {
	if len(pattern) > maxPattern {
		return fmt.Sprintf("search pattern too long (%d characters; max %d)", len(pattern), maxPattern)
	}
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
	const truncated = "[more matches truncated; use a narrower search]\n"
	prev := -2
	for i, k := range keep {
		if !k {
			continue
		}
		sep := ""
		if prev >= 0 && i != prev+1 {
			sep = "--\n"
		}
		if b.Len()+len(sep)+len(lines[i])+1+len(truncated) > limit {
			b.WriteString(truncated)
			break
		}
		prev = i
		b.WriteString(sep)
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
