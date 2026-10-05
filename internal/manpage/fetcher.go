package manpage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var (
	// Page names come from the user or from the model, so keep them to
	// characters that real man pages use and never let them look like flags
	// or paths.
	validName    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+:@-]*$`)
	validSection = regexp.MustCompile(`^[0-9][A-Za-z0-9]{0,3}$|^n$|^l$`)

	ansiRegex = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
	// OSC sequences, e.g. the hyperlinks groff 1.23 emits: ESC ] … (BEL | ESC \)
	oscRegex       = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	backspaceRegex = regexp.MustCompile(`.\x08`)
	blankLines     = regexp.MustCompile(`\n{3,}`)
)

const fetchTimeout = 10 * time.Second

// aproposTimeout is longer: on macOS, apropos rebuilds the whatis index on
// the fly for man directories that lack one (e.g. Homebrew's), which can take
// 5-15s.
const aproposTimeout = 30 * time.Second

// ErrNotFound is returned (wrapped) by Fetch when there is no such man page.
var ErrNotFound = errors.New("man page not found")

// errTimeout is returned when man takes longer than fetchTimeout.
var errTimeout = errors.New("man timed out")

// Fetcher retrieves man pages from the local system.
type Fetcher struct{}

// NewFetcher creates a new man page fetcher.
func NewFetcher() *Fetcher {
	return &Fetcher{}
}

// ValidateName reports whether name is safe to pass to man.
func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid man page name %q", name)
	}
	return nil
}

// Fetch retrieves the man page for the given command, optionally from a
// specific section. Output is plain text (MANPAGER=cat, formatting stripped).
// A missing page yields an error wrapping ErrNotFound.
func (f *Fetcher) Fetch(command string, section string) (string, error) {
	if err := ValidateName(command); err != nil {
		return "", err
	}
	if section != "" {
		if !validSection.MatchString(section) {
			return "", fmt.Errorf("invalid man section %q", section)
		}
		// "man 3 printf" works on macOS and man-db; "-s" is the fallback for
		// other implementations (Solaris/illumos).
		out, err := runMan(section, command)
		if err == nil {
			return out, nil
		}
		if errors.Is(err, errTimeout) {
			return "", fmt.Errorf("man %s %s: %w", section, command, err)
		}
		if out, err2 := runMan("-s", section, command); err2 == nil {
			return out, nil
		}
		return "", fmt.Errorf("%w: %s(%s)", ErrNotFound, command, section)
	}

	out, err := runMan(command)
	if err != nil {
		if errors.Is(err, errTimeout) {
			return "", fmt.Errorf("man %s: %w", command, err)
		}
		return "", fmt.Errorf("%w: %s (try: man -k %s)", ErrNotFound, command, command)
	}
	return out, nil
}

// Apropos searches man page names and descriptions (man -k). No matches is
// not an error: it returns a "nothing appropriate" message.
func (f *Fetcher) Apropos(keyword string) (string, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" || strings.HasPrefix(keyword, "-") || len(keyword) > 100 || strings.ContainsAny(keyword, "\x00\n") {
		return "", fmt.Errorf("invalid search keyword %q", keyword)
	}
	ctx, cancel := context.WithTimeout(context.Background(), aproposTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "man", "-k", "--", keyword)
	// man -k may rebuild the whatis database and chatter on stderr (macOS);
	// never let that reach the user's terminal.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("man -k %s: %w", keyword, errTimeout)
	}
	if text := strings.TrimSpace(string(out)); text != "" {
		return text, nil
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		// man itself couldn't run.
		return "", fmt.Errorf("man -k: %w", err)
	}
	// man -k exits non-zero when nothing matches.
	return fmt.Sprintf("no man pages match %q", keyword), nil
}

// runMan executes man with the given args. Arguments are passed directly to
// exec (no shell), so they cannot inject commands.
func runMan(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "man", args...)
	cmd.Env = manEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", errTimeout
	}
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return "", fmt.Errorf("empty man page")
	}
	return Clean(string(out)), nil
}

// manEnv returns env with the variables that affect man's output replaced so
// the page comes out as plain, 100-column text on stdout. MAN_KEEP_FORMATTING
// (man-db keeps formatting if it is set to anything, even "0") and MANOPT
// (could add e.g. -a and concatenate pages) are removed.
func manEnv(env []string) []string {
	drop := map[string]bool{"MANPAGER": true, "PAGER": true, "MANWIDTH": true, "MAN_KEEP_FORMATTING": true, "MANOPT": true, "GROFF_NO_SGR": true}
	out := make([]string, 0, len(env)+4)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	return append(out, "MANPAGER=cat", "PAGER=cat", "MANWIDTH=100", "GROFF_NO_SGR=1")
}

// ParseCommand parses the command and section from arguments
// Supports:
// - "command question..." → ("command", "", "question...")
// - "3 command question..." → ("command", "3", "question...")
// - "-s 3 command question..." → ("command", "3", "question...")
func ParseCommand(args []string) (command, section string, question []string) {
	if len(args) == 0 {
		return "", "", nil
	}

	if args[0] == "-s" {
		if len(args) >= 3 {
			return args[2], args[1], args[3:]
		}
		return "", "", nil
	}

	// A lone digit is a section number when a command follows it.
	if len(args[0]) == 1 && args[0][0] >= '1' && args[0][0] <= '9' && len(args) >= 2 {
		return args[1], args[0], args[2:]
	}

	return args[0], "", args[1:]
}

// Clean removes overstrike/ANSI formatting and collapses runs of blank lines.
func Clean(content string) string {
	content = oscRegex.ReplaceAllString(content, "")
	content = ansiRegex.ReplaceAllString(content, "")
	// Overstrike: "X\bX" is bold, "_\bX" is underline; drop the char + backspace.
	content = backspaceRegex.ReplaceAllString(content, "")
	content = blankLines.ReplaceAllString(content, "\n\n")
	return strings.TrimSpace(content)
}
