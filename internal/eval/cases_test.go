package eval

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestLoadFixture(t *testing.T) {
	s, err := LoadFile("testdata/cases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Cases) != 3 {
		t.Fatalf("got %d cases", len(s.Cases))
	}
	c := s.Cases[1]
	if c.Form() != "--" || c.Difficulty() != "medium" || !c.HasTag("bsd-gnu") {
		t.Errorf("ps-mem: form=%s diff=%s", c.Form(), c.Difficulty())
	}
	if len(c.Checks[1].Any) != 2 || c.Checks[1].Any[1].Regex == "" {
		t.Errorf("any not decoded: %+v", c.Checks[1])
	}
	if len(c.Platforms["darwin"]) != 1 || len(c.Platforms["linux"]) != 1 {
		t.Errorf("platforms not decoded: %+v", c.Platforms)
	}
	if s.Cases[2].Exec == nil || s.Cases[2].Exec.Compare != CompareStdout || s.Cases[2].Exec.TimeoutSeconds != 5 {
		t.Errorf("exec not decoded: %+v", s.Cases[2].Exec)
	}
	if got := s.Filter(regexp.MustCompile("^bsd-gnu$")); len(got) != 1 || got[0].ID != "ps-mem" {
		t.Errorf("filter by tag: %v", got)
	}
	if got := s.Filter(regexp.MustCompile("lsof")); len(got) != 1 {
		t.Errorf("filter by id: %v", got)
	}
}

// TestLoadRealCases makes sure the shipped dataset parses.
func TestLoadRealCases(t *testing.T) {
	if _, err := os.Stat("../../evals/cases.yaml"); err != nil {
		t.Skip("no evals/cases.yaml")
	}
	if _, err := LoadFile("../../evals/cases.yaml"); err != nil {
		t.Fatal(err)
	}
}

const validCase = `
  - id: a
    question: q
    tags: [easy]
    reference: [ls]
    checks:
      - program: ls
`

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"unknown top-level key", "version: 1\nfoo: 1\ncases:" + validCase, "foo"},
		{"unknown case key", "version: 1\ncases:" + validCase + "    bogus: 1\n", "bogus"},
		{"unknown exec key", "version: 1\ncases:" + validCase + "    exec: {setup: x, compare: stdout, nope: 1}\n", "nope"},
		{"unknown check key", "version: 1\ncases:\n  - id: a\n    question: q\n    tags: [easy]\n    reference: [ls]\n    checks:\n      - prog: ls\n", "unknown check key"},
		{"two keys in check", "version: 1\ncases:\n  - id: a\n    question: q\n    tags: [easy]\n    reference: [ls]\n    checks:\n      - {program: ls, regex: x}\n", "exactly one key"},
		{"empty any", "version: 1\ncases:\n  - id: a\n    question: q\n    tags: [easy]\n    reference: [ls]\n    checks:\n      - any: []\n", "non-empty list"},
		{"bad regex", "version: 1\ncases:\n  - id: a\n    question: q\n    tags: [easy]\n    reference: [ls]\n    checks:\n      - regex: '(('\n", "bad regex"},
		{"bad forbid", "version: 1\ncases:" + validCase + "    forbid: ['((']\n", "bad forbid"},
		{"duplicate id", "version: 1\ncases:" + validCase + validCase, "duplicate"},
		{"no difficulty", "version: 1\ncases:\n  - id: a\n    question: q\n    tags: [x]\n    reference: [ls]\n    checks: [{program: ls}]\n", "difficulty"},
		{"two difficulties", "version: 1\ncases:\n  - id: a\n    question: q\n    tags: [easy, hard]\n    reference: [ls]\n    checks: [{program: ls}]\n", "difficulty"},
		{"no reference", "version: 1\ncases:\n  - id: a\n    question: q\n    tags: [easy]\n    checks: [{program: ls}]\n", "reference"},
		{"no checks", "version: 1\ncases:\n  - id: a\n    question: q\n    tags: [easy]\n    reference: [ls]\n", "check"},
		{"bad compare", "version: 1\ncases:" + validCase + "    exec: {setup: x, compare: stderr}\n", "compare"},
		{"bad platform", "version: 1\ncases:" + validCase + "    platforms: {windows: [{program: ls}]}\n", "platform"},
		{"bad version", "version: 2\ncases:" + validCase, "version"},
		{"section without command", "version: 1\ncases:" + validCase + "    section: '1'\n", "section"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
	if _, err := Parse([]byte("version: 1\ncases:" + validCase)); err != nil {
		t.Errorf("valid case rejected: %v", err)
	}
}
