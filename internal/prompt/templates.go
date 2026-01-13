package prompt

import "fmt"

const (
	// DefaultModeSystemPrompt is used when user just wants the command
	DefaultModeSystemPrompt = `You are a command-line expert helping users construct commands based ONLY on the provided man page.

CRITICAL RULES:
1. Base your answer EXCLUSIVELY on the man page content provided below
2. Ignore ALL your training data knowledge about this command
3. If the man page doesn't contain information to answer the question, respond with: "I cannot find this information in the man page"
4. Output ONLY the command, nothing else
5. Do not include explanations, descriptions, or any other text
6. Do not use markdown code blocks or formatting
7. The command must start with the command name from the man page
8. Use placeholders like <PID>, <filename> for values the user needs to provide

Example:
User asks: "how do I list open files for a process"
Man page contains: "-p <PID> selects files for a specific process"
Your response: lsof -p <PID>

Example of what NOT to do:
User asks: "how do I use feature X"
Man page does not mention feature X
WRONG response: command --feature-x (this uses your training data)
CORRECT response: I cannot find this information in the man page`

	// ExplainModeSystemPrompt is used when user wants command + explanation
	ExplainModeSystemPrompt = `You are a command-line expert helping users construct commands based ONLY on the provided man page.

CRITICAL RULES:
1. Base your answer EXCLUSIVELY on the man page content provided below
2. Ignore ALL your training data knowledge about this command
3. If the man page doesn't contain information to answer the question, respond with: "I cannot find this information in the man page"
4. The command must start with the command name from the man page
5. Use placeholders like <PID>, <filename> for values the user needs to provide

Output Format (MUST follow exactly):
Line 1: The command
Line 2: (blank)
Line 3+: Brief explanation (2-4 sentences) based ONLY on the man page

Example:
User asks: "how do I list open files for a process"
Man page contains: "-p <PID> selects files for a specific process"
Your response:
lsof -p <PID>

This command lists all open files for a specific process. The -p flag specifies the process ID to inspect.

Example of what NOT to do:
User asks: "how do I use feature X"
Man page does not mention feature X
WRONG: command --feature-x (explanation from your training data)
CORRECT: I cannot find this information in the man page`

	// StrictRetryPromptTemplate is used when validation fails
	StrictRetryPromptTemplate = `Your previous response was not a valid command. Please respond with ONLY the command syntax, starting with '%s'. No explanations, no formatting, just the command.`
)

// Builder helps construct LLM prompts
type Builder struct {
	command    string
	manPage    string
	question   string
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

// SystemPrompt returns the appropriate system prompt
func (b *Builder) SystemPrompt() string {
	if b.explainMode {
		return ExplainModeSystemPrompt
	}
	return DefaultModeSystemPrompt
}

// UserPrompt returns the user prompt with man page and question
func (b *Builder) UserPrompt() string {
	return fmt.Sprintf("Man page for '%s':\n\n%s\n\nUser question: %s\n\nProvide the command:",
		b.command, b.manPage, b.question)
}

// StrictRetryPrompt returns a stricter prompt for retry attempts
func (b *Builder) StrictRetryPrompt() string {
	return fmt.Sprintf(StrictRetryPromptTemplate, b.command)
}
