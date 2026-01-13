package parser

import (
	"fmt"
	"regexp"
	"strings"
)

// ParsedResponse represents a parsed LLM response
type ParsedResponse struct {
	Command     string
	Explanation string // Empty in default mode
	Valid       bool
	Error       error
}

// Parser handles parsing and validation of LLM responses
type Parser struct {
	commandName string
	explainMode bool
}

// New creates a new response parser
func New(commandName string, explainMode bool) *Parser {
	return &Parser{
		commandName: commandName,
		explainMode: explainMode,
	}
}

// Parse parses the LLM response and validates it
func (p *Parser) Parse(response string) ParsedResponse {
	response = strings.TrimSpace(response)

	if p.explainMode {
		return p.parseExplainMode(response)
	}
	return p.parseDefaultMode(response)
}

// parseDefaultMode parses response in default mode (command only)
func (p *Parser) parseDefaultMode(response string) ParsedResponse {
	// Check if LLM refused to answer based on man page content
	if strings.Contains(response, "cannot find this information in the man page") {
		return ParsedResponse{
			Valid: false,
			Error: fmt.Errorf("information not found in man page"),
		}
	}

	// Remove markdown code blocks if present
	response = stripMarkdownCodeBlocks(response)
	response = strings.TrimSpace(response)

	// Check if response starts with command name
	if !strings.HasPrefix(response, p.commandName) {
		return ParsedResponse{
			Valid: false,
			Error: fmt.Errorf("response does not start with command '%s'", p.commandName),
		}
	}

	// Check for multi-line responses (invalid in default mode)
	lines := strings.Split(response, "\n")
	if len(lines) > 1 {
		// Take first non-empty line that starts with command
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" && strings.HasPrefix(line, p.commandName) {
				return ParsedResponse{
					Command: line,
					Valid:   true,
				}
			}
		}
		return ParsedResponse{
			Valid: false,
			Error: fmt.Errorf("response contains multiple lines without valid command"),
		}
	}

	return ParsedResponse{
		Command: response,
		Valid:   true,
	}
}

// parseExplainMode parses response in explain mode (command + explanation)
func (p *Parser) parseExplainMode(response string) ParsedResponse {
	// Check if LLM refused to answer based on man page content
	if strings.Contains(response, "cannot find this information in the man page") {
		return ParsedResponse{
			Valid: false,
			Error: fmt.Errorf("information not found in man page"),
		}
	}

	lines := strings.Split(response, "\n")

	if len(lines) == 0 {
		return ParsedResponse{
			Valid: false,
			Error: fmt.Errorf("empty response"),
		}
	}

	// First non-empty line should be the command
	var command string
	var explanationStart int

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Strip markdown if present
		line = stripMarkdownCodeBlocks(line)
		line = strings.TrimSpace(line)

		if command == "" {
			// Check if this line starts with the command name
			if strings.HasPrefix(line, p.commandName) {
				command = line
				explanationStart = i + 1
				break
			}
			// If line doesn't start with command, continue looking
			// (might be preamble text)
			continue
		}
	}

	if command == "" {
		return ParsedResponse{
			Valid: false,
			Error: fmt.Errorf("no command found in response (expected line starting with '%s')", p.commandName),
		}
	}

	// Collect explanation lines
	var explanationLines []string
	for i := explanationStart; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			explanationLines = append(explanationLines, line)
		}
	}

	return ParsedResponse{
		Command:     command,
		Explanation: strings.Join(explanationLines, "\n"),
		Valid:       true,
	}
}

// stripMarkdownCodeBlocks removes markdown code block formatting
func stripMarkdownCodeBlocks(s string) string {
	// Remove ```bash, ```sh, ``` style code blocks
	s = regexp.MustCompile("(?m)^```[a-z]*\\n").ReplaceAllString(s, "")
	s = regexp.MustCompile("(?m)\\n```$").ReplaceAllString(s, "")
	s = strings.TrimPrefix(s, "`")
	s = strings.TrimSuffix(s, "`")
	return strings.TrimSpace(s)
}

// ValidateCommand performs additional validation on extracted command
func (p *Parser) ValidateCommand(command string) error {
	// Check command starts with expected name
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return fmt.Errorf("empty command")
	}

	if parts[0] != p.commandName {
		return fmt.Errorf("command does not start with '%s', got '%s'", p.commandName, parts[0])
	}

	// Basic sanity checks
	if strings.Contains(command, "\n") {
		return fmt.Errorf("command contains newlines")
	}

	if len(command) > 1000 {
		return fmt.Errorf("command suspiciously long (%d chars)", len(command))
	}

	return nil
}
