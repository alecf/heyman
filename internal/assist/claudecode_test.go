package assist

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClaudeArgs(t *testing.T) {
	args := claudeArgs("haiku", "SYSTEM")
	joined := strings.Join(args, "\x00")
	for _, want := range []string{"--system-prompt\x00SYSTEM", "--model\x00haiku", "--permission-mode\x00dontAsk", "--tools\x00Bash", "--settings\x00" + claudeSettings} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %q", want, args)
		}
	}
	// The variadic list flags must be single --flag=value args so they can't
	// swallow anything that follows.
	for i, a := range args {
		if a == "--allowedTools" || a == "--disallowedTools" || a == "--allowed-tools" {
			t.Errorf("args[%d] = %q: variadic flag passed without =", i, a)
		}
	}
	last := args[len(args)-1]
	if !strings.HasPrefix(last, "--allowedTools=") || !strings.Contains(last, "Bash(man:*)") {
		t.Errorf("last arg = %q", last)
	}
	if !strings.Contains(joined, "--disallowedTools=Bash(man -P:*)") {
		t.Error("man -P not denied")
	}
	if strings.Contains(strings.Join(claudeArgs("", "S"), " "), "--model") {
		t.Error("empty model should omit --model")
	}
}

// fakeClaude writes a stand-in for the claude CLI that records its args and
// stdin and prints out.
func fakeClaude(t *testing.T, out string, exit int) (bin, argsFile, stdinFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	stdinFile = filepath.Join(dir, "stdin")
	outFile := filepath.Join(dir, "out")
	if err := os.WriteFile(outFile, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + argsFile + "\ncat > " + stdinFile + "\ncat " + outFile + "\nexit " + string(rune('0'+exit)) + "\n"
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile, stdinFile
}

func TestClaudeCodeAsk(t *testing.T) {
	out := `{"result":"ls -lS\n\nSorts by size.","is_error":false,"num_turns":3,"total_cost_usd":0.01,
"usage":{"input_tokens":10,"output_tokens":5},
"permission_denials":[{"tool_input":{"command":"rm -rf /"}}]}`
	bin, argsFile, stdinFile := fakeClaude(t, out, 0)
	c := &ClaudeCode{Model: "haiku", Binary: bin, Man: &fakeMan{pages: map[string]string{"ls": lsPage}}}

	res, err := c.Ask(context.Background(), Request{Command: "ls", Question: "sort by size"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Command != "ls -lS" || res.Explanation != "" || res.Model != "claude-code/haiku" {
		t.Errorf("res = %+v", res)
	}
	if res.CostUSD == nil || *res.CostUSD != 0.01 || res.Steps != 3 {
		t.Errorf("cost/steps = %v/%d", res.CostUSD, res.Steps)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Error != "denied" {
		t.Errorf("tool calls = %+v", res.ToolCalls)
	}
	stdin, _ := os.ReadFile(stdinFile)
	if string(stdin) != "sort by size" {
		t.Errorf("stdin = %q", stdin)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "Sort by size") {
		t.Error("man page not preloaded into --system-prompt")
	}

	res, err = c.Ask(context.Background(), Request{Command: "ls", Question: "q", Explain: true})
	if err != nil || res.Explanation != "Sorts by size." {
		t.Errorf("explain: %+v %v", res, err)
	}
}

func TestClaudeCodeErrors(t *testing.T) {
	tests := []struct {
		name, out string
		exit      int
		want      error
		msg       string
	}{
		{"cli failure", "boom", 1, nil, "claude CLI failed"},
		{"bad json", "not json", 0, nil, "parsing claude CLI output"},
		{"is_error", `{"result":"rate limited","is_error":true}`, 0, nil, "rate limited"},
		{"refusal", `{"result":"I can't help with that."}`, 0, ErrNoCommand, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, _, _ := fakeClaude(t, tt.out, tt.exit)
			_, err := (&ClaudeCode{Binary: bin, Man: &fakeMan{}}).Ask(context.Background(), Request{Question: "x"})
			if err == nil {
				t.Fatal("want error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if tt.msg != "" && !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("err = %v, want %q", err, tt.msg)
			}
		})
	}
	if _, err := (&ClaudeCode{Binary: "/nonexistent/claude"}).Ask(context.Background(), Request{Question: "x"}); err == nil || !strings.Contains(err.Error(), "PATH") {
		t.Errorf("missing binary: %v", err)
	}
}
