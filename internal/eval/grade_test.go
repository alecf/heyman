package eval

import (
	"strings"
	"testing"
)

func mustSuite(t *testing.T) *Suite {
	t.Helper()
	s, err := LoadFile("testdata/cases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func caseByID(t *testing.T, s *Suite, id string) *Case {
	t.Helper()
	for i := range s.Cases {
		if s.Cases[i].ID == id {
			return &s.Cases[i]
		}
	}
	t.Fatalf("no case %s", id)
	return nil
}

func TestGradeCommand(t *testing.T) {
	s := mustSuite(t)
	tests := []struct {
		id, goos, cand string
		pass           bool
		firstFail      string
	}{
		{"lsof-port", "darwin", "lsof -i :8080", true, ""},
		{"lsof-port", "darwin", "```bash\nlsof -i :8080\n```", true, ""},
		{"lsof-port", "darwin", "`lsof -i :8080`", true, ""},
		{"lsof-port", "darwin", "$ lsof -i :8080", true, ""},
		{"lsof-port", "darwin", "sudo lsof -i :8080", true, ""},
		{"lsof-port", "darwin", "netstat -an | grep 8080", false, "check program: lsof"},
		{"lsof-port", "darwin", "lsof -i :80", false, "check regex"},
		{"lsof-port", "darwin", "echo 'lsof -i :8080'", false, "check program: lsof"},
		{"lsof-port", "darwin", "", false, "check non-empty"},
		// any + platform
		{"ps-mem", "darwin", "ps aux -m | head -n 11", true, ""},
		{"ps-mem", "darwin", "ps aux | sort -nrk 4 | head", true, ""},
		{"ps-mem", "darwin", "ps aux | awk 'NR<=11'", false, "platform:darwin"},
		{"ps-mem", "darwin", "ps aux --sort=-%mem | head", false, "platform:darwin"},
		{"ps-mem", "linux", "ps aux --sort=-%mem | head", true, ""},
		{"ps-mem", "linux", "ps aux -m | head -n 11", false, "platform:linux"},
		{"ps-mem", "freebsd", "ps aux -m | head", true, ""}, // no platform checks for other OSes
		{"ps-mem", "darwin", "ps aux | sort -nrk 4 | wc -l", false, "check any"},
		// forbid
		{"ps-mem", "darwin", "ps aux -m | head -n 11; rm -rf /tmp/x", false, "forbid"},
		// line continuations are joined before grading
		{"ps-mem", "darwin", "ps aux \\\n  -m | head", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.id+"/"+tt.goos+"/"+tt.cand, func(t *testing.T) {
			g := caseByID(t, s, tt.id).GradeCommand(tt.cand, tt.goos)
			if g.Pass != tt.pass {
				t.Fatalf("pass = %v, want %v (%+v)", g.Pass, tt.pass, g)
			}
			if !strings.HasPrefix(g.FirstFailure, tt.firstFail) {
				t.Errorf("first failure %q, want prefix %q", g.FirstFailure, tt.firstFail)
			}
		})
	}
}

func TestCheckEvaluateNestedAny(t *testing.T) {
	c := Check{Any: []Check{
		{Program: "nope"},
		{Any: []Check{{Regex: `x{3}`}, {Program: "awk"}}},
	}}
	if err := c.compile(); err != nil {
		t.Fatal(err)
	}
	if !c.Evaluate("ls | awk 1", Programs("ls | awk 1")) {
		t.Error("nested program should match")
	}
	if !c.Evaluate("echo xxx", Programs("echo xxx")) {
		t.Error("nested regex should match")
	}
	if c.Evaluate("echo xx", Programs("echo xx")) {
		t.Error("should not match")
	}
	if got := c.String(); got != "any: [{program: nope}, {any: [{regex: x{3}}, {program: awk}]}]" {
		t.Errorf("String() = %q", got)
	}
}

func TestNormalizeOutput(t *testing.T) {
	in := "   3 ./a.txt\n\n\t10\t./sub/b  c\n./x\n"
	if got, want := normalizeOutput(in, false), "3 ./a.txt\n10 ./sub/b c\nx"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got, want := normalizeOutput("b\n./a\n", true), "a\nb"; got != want {
		t.Errorf("sorted: got %q want %q", got, want)
	}
}

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		in, verdict string
		err         bool
	}{
		{`{"verdict":"correct","reason":"ok"}`, "correct", false},
		{"```json\n{\"verdict\": \"Incorrect\", \"reason\": \"bad flag\"}\n```", "incorrect", false},
		{`Sure. {"verdict":"incorrect","reason":"x"} hope that helps`, "incorrect", false},
		{`verdict: correct`, "correct", false},
		{`I think it's fine`, "", true},
		{`{"verdict":"maybe"}`, "", true},
	}
	for _, tt := range tests {
		v, _, err := ParseVerdict(tt.in)
		if (err != nil) != tt.err || v != tt.verdict {
			t.Errorf("ParseVerdict(%q) = %q, %v", tt.in, v, err)
		}
	}
}
