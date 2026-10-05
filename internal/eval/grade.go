package eval

import (
	"regexp"
	"strings"
)

// CheckResult is the outcome of one top-level check.
type CheckResult struct {
	Kind  string `json:"kind"` // "check", "platform:<goos>", "forbid"
	Check string `json:"check"`
	Pass  bool   `json:"pass"`
}

// Grade is the outcome of the deterministic checks for one candidate.
type Grade struct {
	Pass   bool          `json:"pass"`
	Checks []CheckResult `json:"checks"`
	// FirstFailure describes the first failing check ("" if all passed).
	FirstFailure string `json:"first_failure,omitempty"`
}

// Normalize applies the dataset's candidate pre-processing: strip code fences,
// surrounding backticks and a leading "$ " prompt, join backslash-newline
// continuations, and trim whitespace.
func Normalize(cmd string) string {
	s := strings.TrimSpace(cmd)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		if first, rest, ok := strings.Cut(s, "\n"); ok && !strings.ContainsAny(strings.TrimSpace(first), " \t") && len(strings.TrimSpace(first)) < 12 {
			s = rest // language tag
		}
	}
	s = strings.TrimSpace(s)
	if len(s) >= 2 && strings.HasPrefix(s, "`") && strings.HasSuffix(s, "`") && strings.Count(s, "`") == 2 {
		s = s[1 : len(s)-1]
	}
	s = strings.TrimPrefix(s, "$ ")
	s = continuation.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

var continuation = regexp.MustCompile(`\\\r?\n[ \t]*`)

// Evaluate reports whether check passes for the (normalized) candidate.
func (c *Check) Evaluate(candidate string, programs []string) bool {
	switch {
	case c.Regex != "":
		re := c.re
		if re == nil {
			re = regexp.MustCompile(c.Regex)
		}
		return re.MatchString(candidate)
	case c.Program != "":
		for _, p := range programs {
			if p == c.Program {
				return true
			}
		}
		return false
	default:
		for i := range c.Any {
			if c.Any[i].Evaluate(candidate, programs) {
				return true
			}
		}
		return false
	}
}

// GradeCommand runs the case's deterministic checks (checks, platform checks
// for goos, forbid) against candidate.
func (c *Case) GradeCommand(candidate, goos string) Grade {
	cand := Normalize(candidate)
	progs := Programs(cand)
	g := Grade{Pass: true}
	add := func(kind, desc string, pass bool) {
		g.Checks = append(g.Checks, CheckResult{Kind: kind, Check: desc, Pass: pass})
		if !pass {
			if g.Pass {
				g.FirstFailure = kind + " " + desc
			}
			g.Pass = false
		}
	}
	if cand == "" {
		add("check", "non-empty command", false)
		return g
	}
	for i := range c.Checks {
		ch := &c.Checks[i]
		add("check", ch.String(), ch.Evaluate(cand, progs))
	}
	for i := range c.Platforms[goos] {
		ch := &c.Platforms[goos][i]
		add("platform:"+goos, ch.String(), ch.Evaluate(cand, progs))
	}
	for i, f := range c.Forbid {
		var re *regexp.Regexp
		if i < len(c.forbid) {
			re = c.forbid[i]
		} else {
			re = regexp.MustCompile(f)
		}
		add("forbid", f, !re.MatchString(cand))
	}
	return g
}
