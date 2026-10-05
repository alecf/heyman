package eval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// Exec statuses.
const (
	ExecPass     = "pass"
	ExecFail     = "fail"
	ExecSkipped  = "skipped"
	ExecRefError = "ref_error" // the reference itself failed: case is broken, not scored
)

// ExecResult is the outcome of an execution check.
type ExecResult struct {
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	RefExit       int    `json:"ref_exit"`
	CandidateExit int    `json:"candidate_exit"`
	RefStdout     string `json:"ref_stdout,omitempty"`
	CandStdout    string `json:"candidate_stdout,omitempty"`
	CandStderr    string `json:"candidate_stderr,omitempty"`
}

// Sandbox kinds.
const (
	SandboxExec  = "sandbox-exec" // macOS
	SandboxBwrap = "bwrap"        // Linux bubblewrap
)

// DetectSandbox returns the sandbox available on this machine, or "".
func DetectSandbox() string {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("sandbox-exec"); err == nil {
			return SandboxExec
		}
	case "linux":
		if _, err := exec.LookPath("bwrap"); err == nil {
			return SandboxBwrap
		}
	}
	return ""
}

// Executor runs reference and candidate commands in sandboxed fixtures.
type Executor struct {
	Sandbox string // from DetectSandbox; "" refuses to execute anything
	// DefaultTimeout applies when a case has no timeout_seconds. Default 10s.
	DefaultTimeout time.Duration
	// MaxOutput caps captured stdout/stderr per command. Default 1 MiB.
	MaxOutput int
}

const execSnippet = 2000

// Run builds the case fixture twice (once per side, at the same path so
// absolute paths match), runs reference[0] and the candidate, and compares.
func (e *Executor) Run(ctx context.Context, c *Case, candidate string) ExecResult {
	return e.run(ctx, c, c.Reference[0], Normalize(candidate))
}

func (e *Executor) run(ctx context.Context, c *Case, reference, candidate string) ExecResult {
	if c.Exec == nil {
		return ExecResult{Status: ExecSkipped, Reason: "no exec block"}
	}
	if e.Sandbox == "" {
		return ExecResult{Status: ExecSkipped, Reason: "no sandbox available (need sandbox-exec on macOS or bwrap on Linux)"}
	}
	if reason := unsafeToRun(candidate); reason != "" {
		return ExecResult{Status: ExecSkipped, Reason: reason}
	}

	timeout := e.DefaultTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if c.Exec.TimeoutSeconds > 0 {
		timeout = time.Duration(c.Exec.TimeoutSeconds) * time.Second
	}

	root, err := os.MkdirTemp("", "heyman-eval-")
	if err != nil {
		return ExecResult{Status: ExecSkipped, Reason: "mkdtemp: " + err.Error()}
	}
	defer os.RemoveAll(root)
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}

	ref, err := e.side(ctx, root, c.Exec.Setup, reference, timeout)
	res := ExecResult{RefExit: ref.exit, RefStdout: snippet(ref.stdout)}
	if err != nil {
		res.Status, res.Reason = ExecRefError, err.Error()
		return res
	}
	if ref.exit != 0 {
		res.Status = ExecRefError
		res.Reason = fmt.Sprintf("reference exited %d: %s", ref.exit, snippet(ref.stderr))
		return res
	}
	refOut := normalizeOutput(ref.stdout, c.Exec.Compare == CompareStdoutSorted)
	if c.Exec.Compare != CompareExit && refOut == "" {
		res.Status, res.Reason = ExecRefError, "reference printed nothing"
		return res
	}
	if ctx.Err() != nil {
		res.Status, res.Reason = ExecSkipped, "interrupted"
		return res
	}

	cand, err := e.side(ctx, root, c.Exec.Setup, candidate, timeout)
	res.CandidateExit = cand.exit
	res.CandStdout = snippet(cand.stdout)
	res.CandStderr = snippet(cand.stderr)
	if err != nil {
		res.Status, res.Reason = ExecRefError, err.Error()
		return res
	}
	switch {
	case cand.timedOut:
		res.Status, res.Reason = ExecFail, fmt.Sprintf("candidate timed out after %s", timeout)
	case cand.exit != 0:
		res.Status, res.Reason = ExecFail, fmt.Sprintf("candidate exited %d", cand.exit)
	case c.Exec.Compare == CompareExit:
		res.Status = ExecPass
	case normalizeOutput(cand.stdout, c.Exec.Compare == CompareStdoutSorted) == refOut:
		res.Status = ExecPass
	default:
		res.Status, res.Reason = ExecFail, "stdout differs from reference"
	}
	return res
}

var sudoRe = regexp.MustCompile(`(^|[\s;&|(])(sudo|doas)\s`)

func unsafeToRun(cmd string) string {
	if strings.TrimSpace(cmd) == "" {
		return "empty command"
	}
	if HasPlaceholder(cmd) {
		return "candidate contains a placeholder"
	}
	if progs := Programs(cmd); slices.Contains(progs, "sudo") || slices.Contains(progs, "doas") || sudoRe.MatchString(cmd) {
		return "candidate uses sudo"
	}
	return ""
}

type sideResult struct {
	exit           int
	stdout, stderr string
	timedOut       bool
}

// side recreates the fixture at root/work, runs setup, then runs cmd.
// A non-nil error means the fixture couldn't be built (setup failed).
func (e *Executor) side(ctx context.Context, root, setup, cmd string, timeout time.Duration) (sideResult, error) {
	work := filepath.Join(root, "work")
	tmp := filepath.Join(root, "tmp")
	for _, d := range []string{work, tmp} {
		if err := os.RemoveAll(d); err != nil {
			return sideResult{}, err
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return sideResult{}, err
		}
	}
	if strings.TrimSpace(setup) != "" {
		r := e.sandboxed(ctx, root, work, tmp, setup, max(timeout, 30*time.Second))
		if r.timedOut || r.exit != 0 {
			return r, fmt.Errorf("setup failed (exit %d): %s", r.exit, snippet(r.stderr))
		}
	}
	return e.sandboxed(ctx, root, work, tmp, cmd, timeout), nil
}

func (e *Executor) sandboxed(ctx context.Context, root, work, tmp, script string, timeout time.Duration) sideResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var argv []string
	switch e.Sandbox {
	case SandboxExec:
		profile := filepath.Join(root, "sandbox.sb")
		if _, err := os.Stat(profile); err != nil {
			if err := os.WriteFile(profile, []byte(sandboxProfile(work, tmp)), 0o444); err != nil {
				return sideResult{exit: -1, stderr: err.Error()}
			}
		}
		argv = []string{"sandbox-exec", "-f", profile, "/bin/bash", "-c", script}
	case SandboxBwrap:
		argv = []string{"bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--bind", work, work, "--bind", tmp, tmp,
			"--unshare-net", "--unshare-pid", "--die-with-parent", "--chdir", work, "--", "/bin/bash", "-c", script}
	default:
		return sideResult{exit: -1, stderr: "no sandbox"}
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = work
	cmd.Env = cleanEnv(work, tmp)
	maxOut := e.MaxOutput
	if maxOut == 0 {
		maxOut = 1 << 20
	}
	stdout := &capWriter{max: maxOut}
	stderr := &capWriter{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	r := sideResult{stdout: stdout.String(), stderr: stderr.String()}
	if ctx.Err() == context.DeadlineExceeded {
		r.timedOut = true
		r.exit = -1
		return r
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.exit = ee.ExitCode()
		if r.exit == -1 {
			r.exit = 128 // killed by signal
		}
	default:
		r.exit = -1
		r.stderr += err.Error()
	}
	return r
}

func sandboxProfile(work, tmp string) string {
	q := func(s string) string { return fmt.Sprintf("%q", s) }
	return `(version 1)
(allow default)
(deny network*)
(deny file-write*)
(allow file-write*
  (subpath ` + q(work) + `)
  (subpath ` + q(tmp) + `)
  (subpath "/dev"))
`
}

func cleanEnv(work, tmp string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + work,
		"TMPDIR=" + tmp,
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
		"PAGER=cat",
		"GIT_PAGER=cat",
		"TERM=dumb",
		"SHELL=/bin/bash",
	}
	for _, k := range []string{"USER", "LOGNAME"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

var spaceRun = regexp.MustCompile(`[ \t]+`)

// normalizeOutput trims each line, collapses runs of spaces/tabs, strips one
// leading "./", drops empty lines, and optionally sorts.
func normalizeOutput(s string, sorted bool) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = spaceRun.ReplaceAllString(strings.TrimSpace(line), " ")
		line = strings.TrimPrefix(line, "./")
		if line != "" {
			lines = append(lines, line)
		}
	}
	if sorted {
		slices.Sort(lines)
	}
	return strings.Join(lines, "\n")
}

func snippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > execSnippet {
		return s[:execSnippet] + "…"
	}
	return s
}

type capWriter struct {
	buf bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (w *capWriter) String() string { return w.buf.String() }
