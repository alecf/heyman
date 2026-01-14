package prompt

import "fmt"

const (
	// StrictRetryPromptTemplate is used when validation fails
	StrictRetryPromptTemplate = `Your previous response was not a valid command. Please respond with ONLY the command syntax, starting with '%s'. No explanations, no formatting, just the command.`
)

// Builder helps construct LLM prompts
type Builder struct {
	command     string
	manPage     string
	question    string
	explainMode bool
}

// NewBuilder creates a new prompt builder
func NewBuilder(command, manPage, question string, explainMode bool) *Builder {
	return &Builder{
		command:     command,
		manPage:     manPage,
		question:    question,
		explainMode: explainMode,
	}
}

// SystemPrompt returns the system prompt with man page and instructions
func (b *Builder) SystemPrompt() string {
	modeInstructions := b.getModeInstructions()

	return fmt.Sprintf(`For the following man page for '%s':

<manpage>
%s
</manpage>

The user will request a specific command line using %s.
%s

Rules:
- Base your answer ONLY on the man page above, not on prior training data
- If the man page doesn't contain enough information to answer, respond with: "I cannot find this information in the man page for %s"
- The command must start with '%s'
- Use placeholders like <PID>, <filename> for values the user needs to provide`,
		b.command, b.manPage, b.command, modeInstructions, b.command, b.command)
}

// getModeInstructions returns mode-specific instructions
func (b *Builder) getModeInstructions() string {
	if b.explainMode {
		return `Respond with the exact command on the first line, then a blank line, then a concise explanation of what the command does and why these flags were chosen.`
	}
	return `Respond with ONLY the exact command, nothing else. No explanation, no markdown, no code blocks.`
}

// UserPrompt returns just the user's question
func (b *Builder) UserPrompt() string {
	return b.question
}

// StrictRetryPrompt returns a stricter prompt for retry attempts
func (b *Builder) StrictRetryPrompt() string {
	return fmt.Sprintf(StrictRetryPromptTemplate, b.command)
}
