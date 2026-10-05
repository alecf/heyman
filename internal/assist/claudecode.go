package assist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/alecf/heyman/internal/manpage"
)

// ClaudeCode answers requests by shelling out to the `claude` CLI in print
// mode. Claude Code reads man pages itself via a Bash tool restricted to
// read-only commands. This lets heyman use a Claude Code login (subscription
// or CLAUDE_CODE_OAUTH_TOKEN) instead of an API key, at the cost of latency.
type ClaudeCode struct {
	// Model is passed to `claude --model` (e.g. "haiku", "sonnet", "claude-haiku-4-5").
	Model string
	// Binary defaults to "claude" on PATH.
	Binary string
	// PreloadChars caps the preloaded man page. Default 48000.
	PreloadChars int
	Man          ManSource
	// OnEvent receives notes (claude's own tool calls aren't streamed).
	OnEvent func(Event)
}

// claudeAllowedTools are the only shell commands Claude Code may run.
// Claude Code checks each part of a pipeline / && chain separately.
var claudeAllowedTools = []string{
	"Bash(man:*)", "Bash(apropos:*)", "Bash(whatis:*)", "Bash(which:*)",
	"Bash(grep:*)", "Bash(head:*)", "Bash(col:*)",
}

// claudeDeniedTools close the obvious holes in the allowlist: `man -P cmd`
// and `man -H cmd` run an arbitrary program. Deny rules are prefix matches,
// so `man -aP cmd` would still get through; the sandbox below is what
// actually contains that.
var claudeDeniedTools = []string{
	"Bash(man -P:*)", "Bash(man --pager:*)", "Bash(man -H:*)", "Bash(man --html:*)",
	"Bash(apropos -P:*)", "Bash(whatis -P:*)",
}

// claudeSettings enables Claude Code's Bash sandbox (Seatbelt on macOS,
// bubblewrap on Linux): commands can read but not write outside the empty
// working directory and have no network, and may not ask to run unsandboxed.
const claudeSettings = `{"sandbox":{"enabled":true,"autoAllowBashIfSandboxed":false,"allowUnsandboxedCommands":false}}`

// claudeArgs builds the claude CLI arguments. --allowedTools and
// --disallowedTools are variadic in the claude CLI, so they're passed as
// single --flag=a,b values and nothing positional follows (the question goes
// on stdin).
func claudeArgs(model, system string) []string {
	args := []string{
		"-p",
		"--output-format", "json",
		"--system-prompt", system,
		"--tools", "Bash",
		"--setting-sources", "",
		"--settings", claudeSettings,
		"--strict-mcp-config",
		"--no-session-persistence",
		"--disable-slash-commands",
		// Deny anything not explicitly allowed, regardless of the user's
		// default permission mode.
		"--permission-mode", "dontAsk",
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args,
		"--disallowedTools="+strings.Join(claudeDeniedTools, ","),
		"--allowedTools="+strings.Join(claudeAllowedTools, ","),
	)
	return args
}

type claudeCodeOutput struct {
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	PermissionDenials []struct {
		ToolInput struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	} `json:"permission_denials"`
}

// Ask implements Answerer.
func (c *ClaudeCode) Ask(ctx context.Context, req Request) (*Result, error) {
	if strings.TrimSpace(req.Question) == "" {
		return nil, fmt.Errorf("no question specified")
	}
	bin := c.Binary
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf("claude-code provider needs the `claude` CLI on PATH: %w", err)
	}
	preloadChars := c.PreloadChars
	if preloadChars == 0 {
		preloadChars = 48000
	}

	man := c.Man
	if man == nil {
		man = manpage.NewFetcher()
	}
	req, preload, missing, err := preparePreload(man, req)
	if err != nil {
		return nil, err
	}
	var pages, notes []string
	if req.Command != "" {
		pages = append(pages, pageKey(req.Command, req.Section))
	}
	if missing != "" {
		notes = append(notes, missingNote(missing))
		if c.OnEvent != nil {
			c.OnEvent(Event{Kind: "note", Detail: notes[0]})
		}
	}

	system := systemPrompt(req, preload, preloadChars, false, missing) + `
You can run read-only shell commands to check man pages on this machine: ` + "`man <page>`, `man <page> | grep -n -A3 -- '<option>'`, `man -k <keyword>`, `which <program>`" + `. Use them whenever you are not certain a flag exists here.
Never run the user's command and never inspect their files or directories: you are only writing the command for them to run. Files the user mentions may not exist here; that's expected.
Your final reply must follow the format above exactly.`

	// Run in an empty directory so project CLAUDE.md files don't leak in.
	dir, err := os.MkdirTemp("", "heyman-claude-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	cmd := exec.CommandContext(ctx, bin, claudeArgs(c.Model, system)...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(req.Question)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return nil, fmt.Errorf("claude CLI failed: %w: %s", err, msg)
	}

	var out claudeCodeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("parsing claude CLI output: %w", err)
	}
	if out.IsError {
		return nil, fmt.Errorf("claude CLI error: %s", out.Result)
	}

	modelName := c.Model
	if modelName == "" {
		modelName = "default"
	}
	res := &Result{
		Model:    "claude-code/" + modelName,
		Notes:    notes,
		Steps:    out.NumTurns,
		ManPages: pages,
		Usage: Usage{
			InputTokens:      out.Usage.InputTokens,
			OutputTokens:     out.Usage.OutputTokens,
			CacheReadTokens:  out.Usage.CacheReadInputTokens,
			CacheWriteTokens: out.Usage.CacheCreationInputTokens,
		},
	}
	cost := out.TotalCostUSD
	res.CostUSD = &cost
	for _, d := range out.PermissionDenials {
		res.ToolCalls = append(res.ToolCalls, ToolCall{Tool: "bash", Input: d.ToolInput.Command, Error: "denied"})
	}
	res.Command, res.Explanation = ParseText(out.Result)
	if !req.Explain {
		res.Explanation = ""
	}
	if res.Command == "" {
		return res, ErrNoCommand
	}
	return res, nil
}
