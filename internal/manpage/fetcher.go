package manpage

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Fetcher handles man page retrieval
type Fetcher struct {
	// Could add caching here in the future if needed
}

// NewFetcher creates a new man page fetcher
func NewFetcher() *Fetcher {
	return &Fetcher{}
}

// Fetch retrieves the man page for the given command
// Supports both "man 3 printf" and "man -s 3 printf" syntax
// Uses MANPAGER=cat and col -b to get clean text output
func (f *Fetcher) Fetch(command string, section string) (string, error) {
	if section != "" {
		// Try both section syntaxes for cross-platform compatibility
		// First try: man <section> <command>
		output, err := f.fetchManPage([]string{section, command})
		if err == nil {
			return cleanManPage(output), nil
		}

		// Second try: man -s <section> <command>
		output, err = f.fetchManPage([]string{"-s", section, command})
		if err != nil {
			return "", fmt.Errorf("man page for %s(%s) not found", command, section)
		}
		return cleanManPage(output), nil
	}

	// No section specified, use default
	output, err := f.fetchManPage([]string{command})
	if err != nil {
		return "", fmt.Errorf("man page for %q not found. Try: man -k %s", command, command)
	}

	return cleanManPage(output), nil
}

// fetchManPage executes man command with given args and pipes through col -b
// This avoids shell injection by using exec.Command with separate arguments
func (f *Fetcher) fetchManPage(args []string) (string, error) {
	// Run man command with MANPAGER=cat to get raw output
	manCmd := exec.Command("man", args...)
	manCmd.Env = append(os.Environ(), "MANPAGER=cat")

	manOutput, err := manCmd.Output()
	if err != nil {
		return "", err
	}

	// Pipe output through col -b to remove backspaces and formatting
	colCmd := exec.Command("col", "-b")
	colCmd.Stdin = bytes.NewReader(manOutput)

	colOutput, err := colCmd.Output()
	if err != nil {
		// If col fails, return the man output anyway
		return string(manOutput), nil
	}

	return string(colOutput), nil
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

	// Check for -s flag
	if args[0] == "-s" && len(args) >= 3 {
		return args[2], args[1], args[3:]
	}

	// Check if first arg is a section number (1-9)
	if len(args[0]) == 1 && args[0][0] >= '1' && args[0][0] <= '9' {
		if len(args) >= 2 {
			return args[1], args[0], args[2:]
		}
		return args[0], "", nil
	}

	// No section, first arg is the command
	if len(args) >= 1 {
		return args[0], "", args[1:]
	}

	return "", "", nil
}

// cleanManPage removes ANSI escape codes and normalizes whitespace
func cleanManPage(content string) string {
	// Remove ANSI escape codes (used for formatting/colors in terminal)
	ansiRegex := regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
	content = ansiRegex.ReplaceAllString(content, "")

	// Remove backspace sequences (used for bold/underline in man pages)
	// Pattern: char + backspace + char
	backspaceRegex := regexp.MustCompile(`.\x08.`)
	content = backspaceRegex.ReplaceAllString(content, "")

	// Normalize excessive blank lines
	content = regexp.MustCompile(`\n{3,}`).ReplaceAllString(content, "\n\n")

	return strings.TrimSpace(content)
}
