package manpage

import (
	"bytes"
	"context"
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

	ansiRegex      = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	backspaceRegex = regexp.MustCompile(`.\x08`)
	blankLines     = regexp.MustCompile(`\n{3,}`)
)

const fetchTimeout = 10 * time.Second

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
func (f *Fetcher) Fetch(command string, section string) (string, error) {
	if err := ValidateName(command); err != nil {
		return "", err
	}
	if section != "" {
		if !validSection.MatchString(section) {
			return "", fmt.Errorf("invalid man section %q", section)
		}
		// "man 3 printf" works on macOS and man-db; "-s" is the fallback for
		// other implementations.
		if out, err := runMan(section, command); err == nil {
			return out, nil
		}
		out, err := runMan("-s", section, command)
		if err != nil {
			return "", fmt.Errorf("man page for %s(%s) not found", command, section)
		}
		return out, nil
	}

	out, err := runMan(command)
	if err != nil {
		return "", fmt.Errorf("man page for %q not found. Try: man -k %s", command, command)
	}
	return out, nil
}

// Apropos searches man page names and descriptions (man -k).
func (f *Fetcher) Apropos(keyword string) (string, error) {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" || strings.HasPrefix(keyword, "-") {
		return "", fmt.Errorf("invalid search keyword %q", keyword)
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "man", "-k", "--", keyword).Output()
	if err != nil && len(out) == 0 {
		return "", fmt.Errorf("no man pages match %q", keyword)
	}
	return strings.TrimSpace(string(out)), nil
}

// runMan executes man with the given args. Arguments are passed directly to
// exec (no shell), so they cannot inject commands.
func runMan(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "man", args...)
	cmd.Env = append(os.Environ(), "MANPAGER=cat", "PAGER=cat", "MANWIDTH=100", "MAN_KEEP_FORMATTING=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return "", fmt.Errorf("empty man page")
	}
	return Clean(string(out)), nil
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
	content = ansiRegex.ReplaceAllString(content, "")
	// Overstrike: "X\bX" is bold, "_\bX" is underline; drop the char + backspace.
	content = backspaceRegex.ReplaceAllString(content, "")
	content = blankLines.ReplaceAllString(content, "\n\n")
	return strings.TrimSpace(content)
}
