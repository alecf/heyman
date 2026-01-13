# heyman

LLM-powered man page Q&A. Ask natural language questions about command-line tools and get exact commands back.

## Features

- 🤖 **Multiple LLM Providers**: OpenAI, Anthropic Claude, and Ollama (local)
- 💾 **Smart Caching**: Responses cached for 30 days (configurable)
- 💰 **Cost Tracking**: Token usage and cost estimates with pricing warnings
- 📋 **Multiple Output Modes**: Plain text, JSON, or copy to clipboard
- ⚡ **Fast**: Cached responses return instantly
- 🎯 **Focused**: Queries the actual man page, not generic knowledge
- 🔧 **Configurable**: Multiple profiles for different providers/models

## Installation

### Homebrew (macOS/Linux)

```bash
brew install alecf/tap/heyman
```

### From Source

```bash
go install github.com/alecf/heyman/cmd/heyman@latest
```

### Download Binary

Download the latest release from [GitHub Releases](https://github.com/alecf/heyman/releases)

## Quick Start

1. **Setup a profile**:
   ```bash
   heyman setup
   ```

2. **Ask a question**:
   ```bash
   heyman ls how do I list files by size
   ```
   Output: `ls -lhS`

3. **Get an explanation**:
   ```bash
   heyman --explain tar how do I create a compressed archive
   ```

## Usage

```
heyman [flags] <command> <question>
```

### Examples

```bash
# Basic usage
heyman grep how do I search recursively

# With explanation
heyman --explain find how do I find files modified today

# Show token usage and costs
heyman --tokens curl how do I download a file

# JSON output
heyman --json ssh how do I connect with a specific port

# Copy to clipboard
heyman --copy ps how do I find a process by name

# Use specific profile
heyman --profile openai-gpt4o lsof list open ports
```

### Flags

- `-e, --explain` - Include explanation (streaming)
- `-j, --json` - JSON output with metadata
- `-t, --tokens` - Show token usage and costs
- `-c, --copy` - Copy command to clipboard
- `-v, --verbose` - Show operation details
- `-d, --debug` - Show full request/response details
- `--no-cache` - Bypass cache for this query
- `-p, --profile` - LLM profile to use

## Configuration

### Setup Wizard

Run the interactive setup wizard:

```bash
heyman setup
```

### Manual Configuration

Edit the config file:

**macOS**: `~/Library/Application Support/heyman/config.toml`
**Linux**: `~/.config/heyman/config.toml`

```toml
default_profile = "ollama-llama"
cache_days = 30

[profiles.ollama-llama]
provider = "ollama"
model = "llama3.2:latest"

[profiles.openai-gpt4o-mini]
provider = "openai"
model = "gpt-4o-mini"

[profiles.anthropic-haiku]
provider = "anthropic"
model = "claude-3-5-haiku-20241022"
```

### Environment Variables

- `HEYMAN_PROFILE` - Override default profile
- `HEYMAN_CACHE_DIR` - Cache location override
- `OPENAI_API_KEY` - OpenAI authentication
- `ANTHROPIC_API_KEY` - Anthropic authentication
- `OLLAMA_HOST` - Ollama server URL (default: http://localhost:11434)

## Providers

### OpenAI

```bash
export OPENAI_API_KEY=sk-...
heyman setup  # Select OpenAI
```

Recommended models:
- `gpt-4o-mini` - Fast and cheap ($0.15 / $0.60 per 1M tokens)
- `gpt-4o` - More capable ($2.50 / $10.00 per 1M tokens)

### Anthropic Claude

```bash
export ANTHROPIC_API_KEY=sk-...
heyman setup  # Select Anthropic
```

Recommended models:
- `claude-3-5-haiku-20241022` - Fast and cheap ($0.80 / $4.00 per 1M tokens)
- `claude-sonnet-4-5-20250924` - High quality ($3.00 / $15.00 per 1M tokens)

### Ollama (Local)

```bash
ollama serve  # Start Ollama
ollama pull llama3.2  # Download a model
heyman setup  # Select Ollama
```

Recommended models:
- `llama3.2:latest` - Fast, good quality
- `deepseek-r1:latest` - Reasoning model
- `llama3.3:70b` - Larger, more capable (requires more RAM)

## Cache Management

View cache statistics:
```bash
heyman cache-stats
```

Clear cache:
```bash
heyman clear-cache
```

Cache is stored in:
- **macOS**: `~/Library/Caches/heyman/`
- **Linux**: `~/.cache/heyman/`

## Profile Management

List all profiles:
```bash
heyman list-profiles
```

Set the default profile:
```bash
heyman set-profile ollama-llama
```

Test configuration:
```bash
heyman test-config
```

## Token Costs

The `--tokens` flag shows usage and estimated costs:

```bash
$ heyman --tokens ls how do I list files by size

ls -lhS

Token usage:
  Input:  8,192 tokens
  Output: 14 tokens
  Total:  8,206 tokens
  Cost:   $0.0007 (estimated, based on 2026-01-12 pricing)

⚠️  Pricing may have changed. Check current rates:
    https://openai.com/api/pricing/
```

**Note**: Prices are estimates. Always check the provider's current pricing page.

## How It Works

1. **Fetch man page**: Executes `man <command>` to get the actual documentation
2. **Build prompt**: Constructs a prompt with the full man page and your question
3. **Query LLM**: Sends to your configured provider (with 8K context window)
4. **Parse response**: Validates and extracts the command
5. **Cache**: Stores the response for future use (30 days by default)

## Troubleshooting

### "No profile configured"

Run `heyman setup` or manually edit the config file.

### "OpenAI API key not found"

Set the environment variable:
```bash
export OPENAI_API_KEY=sk-...
```

### "Ollama API error: EOF"

Make sure Ollama is running:
```bash
ollama serve
```

### Man page not found

The command must have a man page installed:
```bash
man <command>  # Test if it exists
```

## Development

### Build from source

```bash
git clone https://github.com/alecf/heyman
cd heyman
go build -o heyman ./cmd/heyman
```

### Run tests

```bash
go test ./...
```

### Release

Uses [GoReleaser](https://goreleaser.com/):

```bash
git tag v0.1.0
git push origin v0.1.0
goreleaser release
```

## License

MIT License - see [LICENSE](LICENSE) file

## Contributing

Contributions welcome! Please open an issue or PR.

## Credits

Built with:
- [Cobra](https://github.com/spf13/cobra) - CLI framework
- [Viper](https://github.com/spf13/viper) - Configuration
- [OpenAI Go SDK](https://github.com/openai/openai-go)
- [Anthropic Go SDK](https://github.com/anthropics/anthropic-sdk-go)
- [Ollama API](https://github.com/ollama/ollama)
