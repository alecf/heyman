# heyman architecture

heyman is a small CLI around one idea: give the model the man pages from *this* machine through tool calls, and make it answer through a tool, so the flags it returns exist here.

## Request flow

```
argv
 │
 ▼
cli.parseRequest ──► assist.Request{Command, Section, Question, Explain}
 │                     "heyman tar ..."      → Command="tar"
 │                     "heyman 5 crontab ..." → Command="crontab", Section="5"
 │                     "heyman -- ..." / one quoted arg → Command="" (model chooses)
 ▼
cli.resolveModel   --model > HEYMAN_MODEL > --profile/HEYMAN_PROFILE > default_profile > anthropic/claude-haiku-4-5
 │                 llm.ParseSpec → Spec{Provider, Model}
 ▼
cache.Get(key) ──hit──────────────────────────────────────────────┐
 │ miss                                                           │
 ▼                                                                │
assist.New ──► claude-code/… → assist.ClaudeCode (shells out to `claude -p`)
 │         ├─► apple/…       → assist.AppleFM (shells out to `fm respond`)
 │         └─► anything else → assist.Assistant (fantasy agent)   │
 ▼                                                                │
Answerer.Ask                                                      │
 │  preload man page of Command (if any) into the system prompt   │
 │  tool loop (≤10 steps):                                        │
 │     man / man_search / which  ◄──► manpage.Fetcher (`man`)     │
 │     answer(command, explanation) → stop                        │
 ▼                                                                │
cache.Set ────────────────────────────────────────────────────────┤
                                                                  ▼
cli.outputResult: command (+ explanation) on stdout; --json, --tokens, --copy
                  verbose/progress/token output on stderr
```

Code: `internal/cli/root.go` (parse, resolve, cache, output), `internal/assist/assist.go` (prompts, tools, loop), `internal/assist/new.go` (backend selection, `--dry-run` preview), `internal/llm/llm.go` (specs and providers).

Flag parsing stops at the first positional argument (`SetInterspersed(false)`), so heyman's own flags must come before the command name, and things like `-la` after it are part of the question.

## The tool loop

`Assistant.Ask` builds a fantasy agent with a system prompt and four tools. The system prompt includes the OS (with the macOS version from `sw_vers` or `PRETTY_NAME` from `/etc/os-release`), the shell, a warning on macOS that core utilities are BSD, and, when a command was named, its man page. The user message is the request text with nothing added.

| Tool | Does | Limits |
|------|------|--------|
| `man(page, section?, search?, offset?)` | Reads a local man page | Name and section are validated (`manpage.ValidateName`: no leading `-`, no `/`). `man` runs with a 10 s timeout, `MANPAGER=cat` and `MANWIDTH=100`, and the output is stripped of overstrike and ANSI codes. Each result is at most 24,000 chars (`PageChars`), cut at a line boundary, with a header giving the next `offset`. With `search`, it returns matching lines (case-insensitive regex, or a literal if the regex doesn't compile) with 2 lines of context, under the same size cap. Pages are cached for the duration of a request. |
| `man_search(keyword)` | `man -k -- keyword` | First 60 lines; keywords starting with `-` are rejected |
| `which(program)` | `exec.LookPath` | Name validated like a man page name |
| `answer(command, explanation?)` | Records the answer and stops the turn | Rejects an empty command. Rejects a missing explanation when `--explain` is set, so the model calls it again. |

Other limits: the preloaded page is truncated at 48,000 chars (`PreloadChars`), with a note telling the model to read the rest with `offset` or `search`. The loop stops after 10 steps (`MaxSteps`), and each call can produce up to 8,192 output tokens.

If the model never calls `answer`, `ParseText` takes the first code fence, or failing that the first non-empty line, from the final text. If that's empty too, `Ask` returns `ErrNoCommand`.

If the provider rejects tool definitions (the error contains "does not support tools" or similar), `Ask` retries once with no tools and the plain-text prompt variant. That still includes the preloaded page when a command was named.

`Result` records the command, the explanation, the man pages consulted, the tool calls, the step count and token usage summed across steps. `-v` and `--json` show these, and the eval suite uses them.

## claude-code backend

`claude-code/<model>` doesn't go through fantasy. `assist.ClaudeCode` runs:

```
claude -p --output-format json --system-prompt <prompt> --tools Bash \
  --setting-sources "" --strict-mcp-config --no-session-persistence \
  --disable-slash-commands --model <model> \
  --allowedTools "Bash(man:*)" "Bash(apropos:*)" "Bash(whatis:*)" "Bash(which:*)" \
                 "Bash(grep:*)" "Bash(head:*)" "Bash(col:*)"
```

The request goes in on stdin. The process runs in a fresh empty temp directory, so no project `CLAUDE.md` or settings get loaded, and the system prompt tells the model not to run the user's command or look at their files. Bash is the only tool, and only the allowlisted read-only commands are permitted. Denied commands appear in `ToolCalls` with `Error: "denied"`. The model answers in plain text, which is parsed with `ParseText`. Cost and usage come from Claude Code's JSON output (`total_cost_usd`). Expect 10 seconds or more per request.

## apple backend

`apple/system` doesn't go through fantasy either. `assist.AppleFM` runs the `fm` CLI that ships with macOS 27:

```
fm respond --no-stream --greedy --schema <answer schema> --instructions <prompt> --model system
```

The request goes in on stdin. The schema is a generation schema (the format `fm schema object` prints, including `x-order`) with a `command` field, plus `explanation` with `--explain`, so the reply is always JSON. `fm respond` can't register custom tools and the on-device model has a 4,096-token context window, so this backend is single-shot: the named command's man page is cut to 9,000 characters and preloaded, and if `fm` still reports that the context is full, `Ask` retries with half as much (down to 2,000). With `--` there is no page, so the model answers from memory. The schema caps field lengths and each call times out after 45 seconds, because the model sometimes repeats itself until the context fills. `fm` reports no token usage, so it's recorded as 0. Exit status 69 means the machine hasn't accepted the Foundation Models CLI terms (`sudo fm license`).

`fm serve` also offers an OpenAI-compatible endpoint, but it ignores `tools` (the model writes the tool call as text) and adds a server process to manage, so heyman doesn't use it.

## Caching

`cache.GenerateKey` hashes (SHA-256) a key version (`v2`), the model spec, the mode (`command` or `explain`), the command, the section, the question and `runtime.GOOS`. Entries are JSON files in `HEYMAN_CACHE_DIR` (otherwise the XDG cache dir), expire after `cache_days`, and are deleted if they can't be decoded. Bump `keyVersion` in `internal/cache/key.go` whenever the prompt or the `Result` format changes.

## Adding a provider

1. `internal/llm/llm.go`: add a name constant, add its API-key env vars to the `providers` map (this also feeds `--help` and the error messages), and add a case to `NewLanguageModel` that builds the fantasy provider. If the model names are distinctive, add an inference rule to `ParseSpec`.
2. `internal/cli/commands.go`: add an entry to `setupProviders` so `heyman profile setup` offers it.
3. `internal/pricing/database.go`: add prices if you want `--tokens` to show cost. `IsFree` marks local providers.
4. README: add it to the provider table.

A provider that isn't an API (like `claude-code` or `apple`) instead implements `assist.Answerer` and is selected in `assist.New`.
