package assist

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// fakeFM writes a stand-in for the fm CLI. Call n (1-based) prints outs[n-1]
// to stdout, or, if it starts with "ERR:", the rest to stderr and exits with
// status exit. Each call's args go to dir/args.<n> and its stdin to
// dir/stdin.<n>.
func fakeFM(t *testing.T, exit int, outs ...string) (bin, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir = t.TempDir()
	script := "#!/bin/sh\nd=" + dir + "\nn=$(($(cat $d/count 2>/dev/null || echo 0) + 1))\necho $n > $d/count\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > $d/args.$n\ncat > $d/stdin.$n\n" +
		"o=$(cat $d/out.$n)\ncase \"$o\" in ERR:*) printf '%s\\n' \"${o#ERR:}\" >&2; exit " + strconv.Itoa(exit) + ";; esac\nprintf '%s\\n' \"$o\"\n"
	for i, o := range outs {
		if err := os.WriteFile(filepath.Join(dir, "out."+strconv.Itoa(i+1)), []byte(o), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin = filepath.Join(dir, "fm")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAppleSchema(t *testing.T) {
	var s struct {
		Required   []string                  `json:"required"`
		Order      []string                  `json:"x-order"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(appleSchema(false)), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Properties) != 1 || s.Properties["command"] == nil || strings.Join(s.Order, ",") != "command" {
		t.Errorf("schema without explain = %+v", s)
	}
	if err := json.Unmarshal([]byte(appleSchema(true)), &s); err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.Required, ",") != "command,explanation" || strings.Join(s.Order, ",") != "command,explanation" {
		t.Errorf("schema with explain = %+v", s)
	}
}

func TestAppleAsk(t *testing.T) {
	bin, dir := fakeFM(t, 1,
		`{"command": "ls -lS"}`,
		`{"command": "ls -lS", "explanation": "Sorts by size."}`)
	a := &AppleFM{Binary: bin, Man: &fakeMan{pages: map[string]string{"ls": lsPage}}}

	res, err := a.Ask(context.Background(), Request{Command: "ls", Question: "sort by size"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Command != "ls -lS" || res.Explanation != "" || res.Model != "apple/system" || res.Steps != 1 {
		t.Errorf("res = %+v", res)
	}
	if len(res.ManPages) != 1 || res.ManPages[0] != "ls" {
		t.Errorf("man pages = %v", res.ManPages)
	}
	if got := readFile(t, filepath.Join(dir, "stdin.1")); got != "sort by size" {
		t.Errorf("stdin = %q", got)
	}
	args := readFile(t, filepath.Join(dir, "args.1"))
	for _, want := range []string{"respond\n", "--no-stream\n", "--greedy\n", "--schema\n", "--model\nsystem\n", "Sort by size"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}

	res, err = a.Ask(context.Background(), Request{Command: "ls", Question: "q", Explain: true})
	if err != nil || res.Explanation != "Sorts by size." {
		t.Errorf("explain: %+v %v", res, err)
	}
	if !strings.Contains(readFile(t, filepath.Join(dir, "args.2")), "explanation") {
		t.Error("explain schema not requested")
	}
}

func TestAppleRetriesWithShorterPage(t *testing.T) {
	bin, dir := fakeFM(t, 1,
		"ERR:Error: The session's transcript exceeded the model's context size.",
		`{"command": "ls -lS"}`)
	page := lsPage + strings.Repeat("filler line\n", 2000)
	a := &AppleFM{Binary: bin, PreloadChars: 8000, Man: &fakeMan{pages: map[string]string{"ls": page}}}

	res, err := a.Ask(context.Background(), Request{Command: "ls", Question: "sort by size"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Command != "ls -lS" || res.Steps != 2 || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "4000 characters") {
		t.Errorf("res = %+v", res)
	}
	if first, second := readFile(t, filepath.Join(dir, "args.1")), readFile(t, filepath.Join(dir, "args.2")); len(second) >= len(first) {
		t.Errorf("retry prompt not shorter: %d vs %d", len(second), len(first))
	}
}

func TestAppleErrors(t *testing.T) {
	tests := []struct {
		name, out string
		exit      int
		want      error
		msg       string
	}{
		{"cli failure", "ERR:Error: boom", 1, nil, "fm respond failed"},
		{"license", "ERR:You must agree", fmLicenseExit, nil, "sudo fm license"},
		// No man page to shorten, so the context error is final.
		{"context", "ERR:Error: The session's transcript exceeded the model's context size.", 1, errAppleContext, ""},
		{"no command", `{"command": ""}`, 0, ErrNoCommand, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin, _ := fakeFM(t, tt.exit, tt.out)
			_, err := (&AppleFM{Binary: bin, Man: &fakeMan{}}).Ask(context.Background(), Request{Question: "x"})
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
	if _, err := (&AppleFM{Binary: "/nonexistent/fm"}).Ask(context.Background(), Request{Question: "x"}); err == nil || !strings.Contains(err.Error(), "macOS 27") {
		t.Errorf("missing binary: %v", err)
	}
}
