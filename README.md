# heyman

heyman turns a plain-English request into a shell command. Before answering, the model reads the man pages installed on your machine, so the flags it uses are the ones your system actually has.

```console
$ heyman lsof which process is listening on port 8080
lsof -i :8080

$ heyman stat print the size in bytes and modification time of notes.txt
stat -f "%z %Sm" notes.txt
```

That second answer is the BSD `stat` that ships with macOS. On Linux the same request gets GNU syntax (`stat -c ...`), because the model read a different man page.

If you don't know which program to use, put `--` before the request and let heyman pick. `-v` shows the man pages and tool calls it used (on stderr):

```console
$ heyman -v -- compress the logs directory into a tarball, excluding .tmp files
Model: anthropic/claude-haiku-4-5 (default)
Request: compress the logs directory into a tarball, excluding .tmp files
  → man tar search="exclude"
  → answer {"command":"tar --exclude='*.tmp' -czf logs.tar.gz logs", ...}
Man pages consulted: tar
Steps: 2, tokens in/out: 3051/297
tar --exclude='*.tmp' -czf logs.tar.gz logs
```

`--explain` adds an explanation:

```console
$ heyman --explain find files modified in the last 2 days larger than 10MB
find . -mtime -2 -size +10M

This command finds files in the current directory and subdirectories that:
- `-mtime -2`: were modified within the last 2 days (the minus sign means "less than")
- `-size +10M`: are larger than 10 megabytes (the plus sign means "more than")

The results will be printed to stdout by default.
```

Only the command goes to stdout, so `$(heyman ...)` and pipes get just the command. Progress, verbose output and token counts go to stderr.

## How it works

When you name a program (`heyman tar ...`), its man page from this machine goes into the prompt. The model can also call three tools while it works out the answer:

- `man`: read any man page on this machine, in chunks or filtered by a search pattern
- `man_search`: search man page names and descriptions, like `man -k`
- `which`: check whether a program is installed

It finishes by calling `answer` with the command. With `--`, nothing is preloaded and the model uses `man_search` and `man` to find the right program.

The model reads man pages through these tools instead of relying on what it remembers, so the flags match your OS: BSD `sed`, `date`, `find`, `stat` and `xargs` on macOS, GNU versions on Linux. The prompt also tells the model which OS and shell you're using.

See [docs/architecture.md](docs/architecture.md) for details.

## Install

With Go 1.27 or later:

```bash
go install github.com/alecf/heyman/cmd/heyman@latest
```

From source:

```bash
git clone https://github.com/alecf/heyman
cd heyman
go build -o bin/heyman ./cmd/heyman
```

Release builds are set up with GoReleaser to publish binaries on [GitHub Releases](https://github.com/alecf/heyman/releases) and a Homebrew cask in [alecf/homebrew-tap](https://github.com/alecf/homebrew-tap). No release has been published yet. After the first one, you'll be able to install with:

```bash
brew install --cask alecf/tap/heyman
```

heyman needs `man` on your PATH. It works on macOS and Linux.

## Usage

```
heyman [flags] <command> <request>
heyman [flags] <section> <command> <request>
heyman [flags] -- <request>
heyman [flags] "<whole request in one quoted argument>"
```

- `heyman tar extract foo.tgz into /tmp`: names the program, and its man page is preloaded.
- `heyman 5 crontab run a job every weekday at 9am`: reads the man page from a specific section.
- `heyman -- find duplicate files`: no program named, so the model chooses. A single quoted argument containing spaces does the same thing: `heyman "find duplicate files"`.

**Flags must come before the command name.** Anything after the command name belongs to the request, so `heyman ls -la what does this do` asks about `-la` and doesn't pass it as a flag.

### Flags

| Flag | Description |
|------|-------------|
| `-m, --model provider/model` | Model to use (see [Models](#models)) |
| `-p, --profile name` | Use a named profile from the config file |
| `-e, --explain` | Include an explanation |
| `-j, --json` | JSON output: `command`, `explanation`, and `metadata` (model, man pages, steps, tokens, cached, cost) |
| `-c, --copy` | Copy the command to the clipboard (`pbcopy`, `wl-copy` or `xclip`) |
| `-t, --tokens` | Show token usage and cost on stderr |
| `-v, --verbose` | Show the model, man pages consulted and tool calls on stderr |
| `-d, --debug` | Like `-v`, plus the full tool trace |
| `-q, --quiet` | No spinner |
| `--no-cache` | Skip the cache for this query |
| `--dry-run` | Print the prompt without calling a model |
| `--version` | Print the version |

## Models

Pick a model with `--model provider/model`. heyman checks these in order and uses the first one that's set:

1. `--model`
2. the `HEYMAN_MODEL` environment variable
3. `--profile`, or `HEYMAN_PROFILE`
4. `default_profile` in the config file
5. the built-in default, `anthropic/claude-haiku-4-5`

| Provider | Credentials | Example |
|----------|-------------|---------|
| `anthropic` | `ANTHROPIC_API_KEY` or `CLAUDE_API_KEY` | `anthropic/claude-haiku-4-5`, `anthropic/claude-sonnet-5-5` |
| `openai` | `OPENAI_API_KEY` | `openai/gpt-...` |
| `openrouter` | `OPENROUTER_API_KEY` | `openrouter/<org>/<model>` |
| `google` | `GEMINI_API_KEY` or `GOOGLE_API_KEY` | `google/gemini-...` |
| `ollama` | none; `OLLAMA_HOST` is optional (default `localhost:11434`) | `ollama/qwen3:4b` |
| `openai-compat` | `HEYMAN_OPENAI_COMPAT_BASE_URL` (or `base_url` in a profile), plus `HEYMAN_OPENAI_COMPAT_API_KEY` if the server needs a key | `openai-compat/<model>` |
| `claude-code` | the `claude` CLI, installed and logged in | `claude-code/haiku` |

You can leave out the provider when the model name makes it obvious. Names starting with `claude-` go to Anthropic, `gpt-`, `chatgpt-` and `o1`-style names go to OpenAI, and `gemini-` goes to Google. A `name:tag` name like `qwen3:4b` goes to Ollama. Anything else needs the `provider/` prefix.

For open-weight models, you can run them locally with Ollama, use a hosted one through OpenRouter, or point `openai-compat` at any OpenAI-compatible server (llama.cpp, vLLM, LM Studio, ...).

Some notes on local models:

- The model needs tool-calling support to read man pages. If the server rejects tool definitions, heyman retries once without tools, with just the named command's man page in the prompt. With `--` there's no page to include, so the model answers from memory. Very small models often give wrong answers either way.
- Ollama's default context window can be small. heyman preloads up to about 48,000 characters of the named man page, and the `man` tool returns at most 24,000 characters per call, sending longer pages in chunks. If answers look like the model missed part of the page, raise the limit on the server, e.g. `OLLAMA_CONTEXT_LENGTH=32768 ollama serve`.

`claude-code` runs `claude -p` and uses your Claude Code login, so you don't need an API key. Claude Code can run only read-only commands (`man`, `apropos`, `whatis`, `which`, `grep`, `head`, `col`), and it runs in an empty temporary directory. Expect it to take 10 seconds or more per request.

## Configuration

The config file lives at:

- macOS: `~/Library/Application Support/heyman/config.toml`
- Linux: `~/.config/heyman/config.toml` (or `$XDG_CONFIG_HOME/heyman/config.toml`)

```toml
default_profile = "haiku"
cache_days = 30

[profiles.haiku]
provider = "anthropic"
model = "claude-haiku-4-5"

[profiles.sonnet]
provider = "anthropic"
model = "claude-sonnet-5-5"

[profiles.local]
provider = "ollama"
model = "qwen3:4b"

[profiles.llamacpp]
provider = "openai-compat"
model = "my-model"
base_url = "http://localhost:8080/v1"
```

`base_url` is the OpenAI-compatible endpoint, including the `/v1` path. It's required for `openai-compat`. For `ollama`, it's optional and replaces the URL heyman builds from `OLLAMA_HOST`. `base_url` only applies when the model comes from a profile, not from `--model` or `HEYMAN_MODEL`. Put API keys in environment variables, not in the config file.

### Profile commands

```bash
heyman profile setup              # interactive wizard; writes a profile to the config file
heyman profile list               # list profiles; * marks the default
heyman profile show [name]        # show a profile (default profile if no name)
heyman profile set-default <name>
heyman profile delete <name>      # -f skips the confirmation
heyman test-config                # check each profile can be constructed (keys, base URLs)
```

`test-config` doesn't call the model. It only checks that the provider can be set up with the credentials and URLs it has.

### Cache

Answers are cached on disk for `cache_days` (default 30). The cache key covers the model, the request, the named command and section, whether `--explain` was used, and the OS. Changing any of these sends a new request.

```bash
heyman cache-stats    # entries, size, hits, location
heyman clear-cache
heyman --no-cache ... # bypass for one query
```

The default location is `~/Library/Caches/heyman` on macOS and `~/.cache/heyman` on Linux. Set `HEYMAN_CACHE_DIR` to use a different directory.

### Environment variables

| Variable | Purpose |
|----------|---------|
| `HEYMAN_MODEL` | Model spec. Overrides profiles. |
| `HEYMAN_PROFILE` | Profile name. Overrides `default_profile`. |
| `HEYMAN_CACHE_DIR` | Cache directory |
| `HEYMAN_OPENAI_COMPAT_BASE_URL`, `HEYMAN_OPENAI_COMPAT_API_KEY` | Endpoint and key for `openai-compat` |
| `OLLAMA_HOST` | Ollama server address |
| Provider keys | See the table in [Models](#models) |

## Costs

`--tokens` prints token usage, totaled over every step of the tool loop, to stderr:

```console
$ heyman -t -- compress the logs directory into a tarball, excluding .tmp files
tar --exclude='*.tmp' -czf logs.tar.gz logs

Token usage:
  Input:  3,051 tokens
  Output: 297 tokens
  Total:  3,348 tokens
  Cost:   $0.0045 (estimated from 2026-09-25 list prices; check https://www.anthropic.com/pricing#api)
```

heyman only has prices for Anthropic models. Their cost is estimated from list prices built into the binary, and it doesn't account for prompt-caching discounts. `claude-code` reports its own cost. Ollama shows as free. For every other provider, the cost shows as unknown. A typical request on the default model costs about half a cent.

## Evals

The [`evals/`](evals/) directory has an eval suite that runs a set of requests against one or more models and checks the commands they return. Run it with `go run ./cmd/heyman-eval --models ...` or `make eval`. See [evals/README.md](evals/README.md) for the case format and options.

## Development

```bash
go build -o bin/heyman ./cmd/heyman   # or: make build
go test -race ./...
go vet ./...
```

Layout:

| Path | Contents |
|------|----------|
| `cmd/heyman` | `main`; version info is injected with ldflags |
| `internal/cli` | Cobra commands, argument parsing, model resolution, output |
| `internal/assist` | Prompts, the tool loop (`man`, `man_search`, `which`, `answer`) and the `claude-code` backend |
| `internal/llm` | `provider/model` parsing and provider construction on [fantasy](https://github.com/charmbracelet/fantasy) |
| `internal/manpage` | Runs `man` / `man -k`, validates names, strips formatting |
| `internal/cache` | On-disk answer cache |
| `internal/config` | TOML config and profiles |
| `internal/pricing` | Built-in Anthropic prices |
| `internal/output` | JSON output and clipboard |

[docs/architecture.md](docs/architecture.md) covers the request flow, the tools and how to add a provider.

## License

MIT
