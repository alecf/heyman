package manpage

import (
	"fmt"
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
	var cmd *exec.Cmd

	if section != "" {
		// Try both section syntaxes for cross-platform compatibility
		// First try: man <section> <command>
		cmd = exec.Command("sh", "-c", fmt.Sprintf("MANPAGER=cat man %s %s | col -b", section, command))
		output, err := cmd.Output()
		if err == nil {
			return cleanManPage(string(output)), nil
		}

		// Second try: man -s <section> <command>
		cmd = exec.Command("sh", "-c", fmt.Sprintf("MANPAGER=cat man -s %s %s | col -b", section, command))
		output, err = cmd.Output()
		if err != nil {
			return "", fmt.Errorf("man page for %s(%s) not found", command, section)
		}
		return cleanManPage(string(output)), nil
	}

	// No section specified, use default
	cmd = exec.Command("sh", "-c", fmt.Sprintf("MANPAGER=cat man %s | col -b", command))
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("man page for %q not found. Try: man -k %s", command, command)
	}

	return cleanManPage(string(output)), nil
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
