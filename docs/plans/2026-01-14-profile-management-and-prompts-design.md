# Profile Management and Prompt Improvements Design

> **Historical.** This January 2026 design predates the October 2026 rebuild on fantasy with man-page tool calling. The profile commands it describes still exist, but the single-shot prompt design (no tools, strict retry prompt) and the provider list are out of date. See [docs/architecture.md](../architecture.md) for how heyman works now.

## Overview

Four improvements to heyman:
1. Profile deletion capability
2. Smart naming to avoid collisions
3. Cleaner default names (no provider prefix)
4. Restructured prompts for better user instruction emphasis

## 1. Profile Command Group

Replace `heyman setup` with a unified `heyman profile` command group:

| Command | Description |
|---------|-------------|
| `heyman profile list` | List all profiles, mark default with `*` |
| `heyman profile setup` | Interactive wizard to create new profile |
| `heyman profile delete <name>` | Delete a profile |
| `heyman profile set-default <name>` | Set the default profile |
| `heyman profile show [name]` | Show profile details (defaults to current) |

### Default Protection

- Always auto-select a new default when deleting the current default
- If only one profile remains, it becomes default automatically
- If multiple remain after deleting default, pick the first alphabetically and notify user
- `heyman profile setup` sets new profile as default if it's the first one

### Example Output

```
$ heyman profile list
* gemma3      ollama   gemma3:1b
  llama3      ollama   llama3.2:latest
  gpt4        openai   gpt-4o-mini
```

## 2. Smart Profile Naming

### Priority Order (for collision avoidance)

1. Just the model base name: `gemma3`
2. Add size/version suffix: `gemma3-1b`
3. Add provider prefix: `ollama-gemma3-1b`

### Smart Tag Parsing

For Ollama models like `gemma3:1b-instruct-q4_0`:
- Extract base name: `gemma3`
- Extract size (prioritized): `1b`, `7b`, `70b`, `270b`
- Skip noise: `latest`, `instruct`, quantization (`q4_0`, `q8`)

### Collision Resolution Example

```
Input: gemma3:1b (and "gemma3" already exists)
1. Try "gemma3" → exists
2. Try "gemma3-1b" → available, use it

Input: gemma3:1b (and both "gemma3" AND "gemma3-1b" exist)
1. Try "gemma3" → exists
2. Try "gemma3-1b" → exists
3. Try "ollama-gemma3-1b" → available, use it
```

### Edge Cases

- `gemma3:latest` → tries `gemma3`, then `ollama-gemma3` (no useful suffix)
- Cloud providers: Same logic, e.g., `gpt-4o-mini` → `openai-gpt-4o-mini`

## 3. Prompt Restructure

### New System Prompt Structure

```
For the following man page for '{command}':

<manpage>
{full man page content}
</manpage>

The user will request a specific command line using {command}.
{mode-specific instructions}

Rules:
- Base your answer ONLY on the man page above, not on prior training data
- If the man page doesn't contain enough information to answer, respond with:
  "I cannot find this information in the man page for {command}"
```

### Mode-Specific Instructions

**Default mode:**
```
Respond with ONLY the exact command, nothing else. No explanation, no markdown, no code blocks.
```

**Explain mode (`--explain`):**
```
Respond with the exact command on the first line, then a blank line, then a concise explanation of what the command does and why these flags were chosen.
```

### User Message

Just the user's question, nothing else:
```
How do I recursively search for "foo" in all .txt files?
```

## Files to Modify

- `internal/config/config.go` - Add `DeleteProfile()`, `SetDefault()`
- `internal/cli/commands.go` - Replace `setup` with `profile` command group
- `internal/cli/root.go` - Update command registration
- `internal/prompt/templates.go` - Restructure system/user prompts

## Implementation Order

1. Profile command group (structural change)
2. Smart naming (builds on profile commands)
3. Prompt restructure (independent, can be done in parallel)
