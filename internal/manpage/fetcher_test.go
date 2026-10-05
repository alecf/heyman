package manpage

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"ls", "git-log", "c++", "perl5.30", "_exit", "systemd.unit", "user@.service", "pg_dump"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-P", "--pager=sh", "../etc/passwd", "/bin/ls", "a b", "ls;rm", "$(id)", ".hidden", "a\nb"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", bad)
		}
	}
}

func TestFetchRejectsBadInput(t *testing.T) {
	f := NewFetcher()
	if _, err := f.Fetch("-P", ""); err == nil {
		t.Error("flag-like name accepted")
	}
	for _, sec := range []string{"-P", "1;", "abc", "12345"} {
		if _, err := f.Fetch("ls", sec); err == nil || !strings.Contains(err.Error(), "invalid man section") {
			t.Errorf("section %q: err = %v", sec, err)
		}
	}
	for _, kw := range []string{"", "-P", strings.Repeat("x", 101), "a\nb"} {
		if _, err := f.Apropos(kw); err == nil {
			t.Errorf("Apropos(%q): want error", kw)
		}
	}
}

func TestParseCommand(t *testing.T) {
	tests := []struct {
		args          []string
		cmd, sec, que string
	}{
		{nil, "", "", ""},
		{[]string{"ls"}, "ls", "", ""},
		{[]string{"ls", "sort", "by", "size"}, "ls", "", "sort by size"},
		{[]string{"3", "printf", "what"}, "printf", "3", "what"},
		{[]string{"-s", "3", "printf", "what"}, "printf", "3", "what"},
		{[]string{"-s", "3"}, "", "", ""},
		{[]string{"3"}, "3", "", ""},
		{[]string{"7z", "extract"}, "7z", "", "extract"},
	}
	for _, tt := range tests {
		cmd, sec, q := ParseCommand(tt.args)
		if cmd != tt.cmd || sec != tt.sec || strings.Join(q, " ") != tt.que {
			t.Errorf("ParseCommand(%q) = %q, %q, %q", tt.args, cmd, sec, q)
		}
	}
}

func TestClean(t *testing.T) {
	tests := map[string]string{
		"N\bNA\bAM\bME\bE":                          "NAME",
		"_\bf_\bi_\bl_\be":                          "file",
		"\x1b[1mbold\x1b[0m":                        "bold",
		"\x1b[?25lhidden":                           "hidden",
		"\x1b]8;;https://x\x1b\\link\x1b]8;;\x1b\\": "link",
		"\x1b]8;;https://x\alink":                   "link",
		"a\n\n\n\n\nb":                              "a\n\nb",
		"l\bls\bs \xe2\x80\x93\b\xe2\x80\x93 list":  "ls – list",
		"  keep  \n":                                "keep",
	}
	for in, want := range tests {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestManEnv(t *testing.T) {
	env := manEnv([]string{"HOME=/h", "MANPAGER=less", "MAN_KEEP_FORMATTING=0", "MANOPT=-a", "PAGER=more", "MANWIDTH=40"})
	for _, bad := range []string{"MANPAGER=less", "MAN_KEEP_FORMATTING=0", "MANOPT=-a", "PAGER=more", "MANWIDTH=40"} {
		if slices.Contains(env, bad) {
			t.Errorf("env still has %s", bad)
		}
	}
	for _, want := range []string{"HOME=/h", "MANPAGER=cat", "PAGER=cat", "MANWIDTH=100"} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %s", want)
		}
	}
}

// Integration tests against the real man; skipped where it isn't installed.
func TestFetchReal(t *testing.T) {
	if _, err := exec.LookPath("man"); err != nil {
		t.Skip("no man")
	}
	f := NewFetcher()
	page, err := f.Fetch("ls", "")
	if err != nil {
		t.Skipf("no ls man page here: %v", err)
	}
	if strings.Contains(page, "\b") || strings.Contains(page, "\x1b") || !strings.Contains(strings.ToLower(page), "list") {
		t.Errorf("ls page not clean: %q", page[:min(200, len(page))])
	}
	if _, err := f.Fetch("zzz-no-such-page-heyman", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing page err = %v, want ErrNotFound", err)
	}
	if _, err := f.Fetch("ls", "9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("wrong section err = %v, want ErrNotFound", err)
	}
	if testing.Short() {
		return // apropos can take many seconds on macOS
	}
	out, err := f.Apropos("zzznosuchkeywordheyman")
	if errors.Is(err, errTimeout) {
		t.Skip("apropos timed out")
	}
	if err != nil || !strings.Contains(out, "no man pages match") {
		t.Errorf("Apropos no match = %q, %v", out, err)
	}
}
