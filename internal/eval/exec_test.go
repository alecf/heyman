package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecRefusals(t *testing.T) {
	c := &Case{ID: "x", Reference: []string{"echo hi"}, Exec: &ExecSpec{Compare: CompareStdout}}
	none := &Executor{}
	if r := none.Run(context.Background(), c, "echo hi"); r.Status != ExecSkipped || !strings.Contains(r.Reason, "sandbox") {
		t.Errorf("no sandbox: %+v", r)
	}
	ex := &Executor{Sandbox: "fake"}
	for _, cand := range []string{"cat <file>", "sudo ls", "ls | sudo tee x", ""} {
		if r := ex.Run(context.Background(), c, cand); r.Status != ExecSkipped {
			t.Errorf("%q: %+v", cand, r)
		}
	}
}

func TestExecSandboxed(t *testing.T) {
	sb := DetectSandbox()
	if sb == "" {
		t.Skip("no sandbox on this machine")
	}
	ex := &Executor{Sandbox: sb}
	mk := func(compare, ref string) *Case {
		return &Case{ID: "t", Reference: []string{ref}, Exec: &ExecSpec{
			Setup:          "mkdir -p sub; printf 'a\\nb\\n' > a.txt; printf 'c\\n' > 'sub/b c.txt'",
			Compare:        compare,
			TimeoutSeconds: 3,
		}}
	}
	ctx := context.Background()
	tests := []struct {
		name, compare, ref, cand, status string
	}{
		{"same output", CompareStdout, "cat a.txt sub/*.txt | wc -l", "find . -name '*.txt' -exec cat {} + | wc -l", ExecPass},
		{"padding normalized", CompareStdout, "printf '3\\n'", "printf '   3   \\n'", ExecPass},
		{"different output", CompareStdout, "cat a.txt", "cat 'sub/b c.txt'", ExecFail},
		{"sorted", CompareStdoutSorted, "find . -type f", "find . -type f | sort -r", ExecPass},
		{"unsorted differs", CompareStdout, "printf 'a\\nb\\n'", "printf 'b\\na\\n'", ExecFail},
		{"candidate nonzero exit", CompareExit, "true", "false", ExecFail},
		{"exit mode", CompareExit, "true", "ls >/dev/null", ExecPass},
		{"ref prints nothing", CompareStdout, "true", "echo x", ExecRefError},
		{"ref fails", CompareStdout, "exit 3", "echo x", ExecRefError},
		{"timeout", CompareExit, "true", "sleep 30", ExecFail},
		{"HOME is fixture", CompareStdout, "pwd", "cd ~ && pwd", ExecPass},
		{"absolute paths match", CompareStdout, "echo \"$PWD\"/a.txt", "find \"$PWD\" -name a.txt", ExecPass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ex.Run(ctx, mk(tt.compare, tt.ref), tt.cand)
			if r.Status != tt.status {
				t.Errorf("status %s (%s), want %s; ref=%q cand=%q stderr=%q", r.Status, r.Reason, tt.status, r.RefStdout, r.CandStdout, r.CandStderr)
			}
		})
	}
}

func TestExecSandboxBlocksWritesAndNetwork(t *testing.T) {
	sb := DetectSandbox()
	if sb == "" {
		t.Skip("no sandbox on this machine")
	}
	home, _ := os.UserHomeDir()
	target := filepath.Join(home, ".heyman-eval-sandbox-test")
	_ = os.Remove(target)
	ex := &Executor{Sandbox: sb}
	c := &Case{ID: "t", Reference: []string{"true"}, Exec: &ExecSpec{Compare: CompareExit, TimeoutSeconds: 5}}
	r := ex.Run(context.Background(), c, "echo pwned > "+target)
	if _, err := os.Stat(target); err == nil {
		os.Remove(target)
		t.Fatal("sandbox allowed a write outside the fixture")
	}
	if r.Status != ExecFail {
		t.Errorf("write outside fixture should fail: %+v", r)
	}
	r = ex.Run(context.Background(), c, "curl -sS -m 3 -o /dev/null https://example.com")
	if r.Status != ExecFail {
		t.Errorf("network should be blocked: %+v", r)
	}
}
