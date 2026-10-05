package eval

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// SelfTestIssue is one problem found by SelfTest.
type SelfTestIssue struct {
	CaseID   string
	Severity string // "error" (reference[0] or case is broken) or "warn"
	Message  string
}

// ManChecker is the part of a man page source SelfTest needs.
type ManChecker interface {
	Fetch(name, section string) (string, error)
}

// SelfTest grades every case's references instead of calling a model:
// reference[0] must pass all checks on goos (error), other references should
// too (warning). With an executor, exec cases also run reference[0] against
// itself (the setup must work and the output must be non-empty) and the other
// references against reference[0] (warning on mismatch). With man, it checks
// that each case's `command` has a man page here.
func SelfTest(ctx context.Context, cases []*Case, goos string, ex *Executor, man ManChecker, parallel int) []SelfTestIssue {
	if parallel < 1 {
		parallel = 4
	}
	var (
		mu     sync.Mutex
		issues []SelfTestIssue
		wg     sync.WaitGroup
		sem    = make(chan struct{}, parallel)
	)
	add := func(c *Case, sev, format string, args ...any) {
		mu.Lock()
		issues = append(issues, SelfTestIssue{CaseID: c.ID, Severity: sev, Message: fmt.Sprintf(format, args...)})
		mu.Unlock()
	}
	for _, c := range cases {
		if reason := SkipReason(c, goos); reason != "" {
			add(c, "warn", "skipped on this machine: %s", reason)
			continue
		}
		for i, ref := range c.Reference {
			g := c.GradeCommand(ref, goos)
			if g.Pass {
				continue
			}
			sev := "warn"
			if i == 0 {
				sev = "error"
			}
			add(c, sev, "reference[%d] fails %s\n      ref: %s", i, g.FirstFailure, ref)
		}
		// Per-OS references are graded as that OS, whatever the host is.
		for refOS, refs := range c.ReferencesByOS {
			for i, ref := range refs {
				if g := c.GradeCommand(ref, refOS); !g.Pass {
					add(c, "error", "references_by_os.%s[%d] fails %s\n      ref: %s", refOS, i, g.FirstFailure, ref)
				}
			}
		}
		if man != nil && c.Command != "" {
			if _, err := man.Fetch(c.Command, c.Section); err != nil {
				add(c, "error", "no man page for command %q: %v", c.Command, err)
			}
		}
		if ex == nil || c.Exec == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(c *Case) {
			defer wg.Done()
			defer func() { <-sem }()
			r := ex.run(ctx, c, c.Reference[0], Normalize(c.Reference[0]))
			switch r.Status {
			case ExecPass:
			case ExecSkipped:
				add(c, "warn", "exec skipped for reference[0]: %s", r.Reason)
				return
			default:
				add(c, "error", "exec of reference[0] against itself: %s %s", r.Status, r.Reason)
				return
			}
			for i, ref := range c.Reference[1:] {
				rr := ex.run(ctx, c, c.Reference[0], Normalize(ref))
				if rr.Status != ExecPass {
					add(c, "warn", "reference[%d] exec %s: %s\n      ref: %s\n      got: %s", i+1, rr.Status, rr.Reason, ref, firstLines(rr.CandStdout, 3))
				}
			}
		}(c)
	}
	wg.Wait()
	return issues
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.Join(lines, " ⏎ ")
}
