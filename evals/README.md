# heyman evals

`evals/cases.yaml` is a set of natural-language requests with known-good answers
and grading rules. `cmd/heyman-eval` runs one or more models over it through the
same code path as `heyman` (`assist.New` → `Ask`, with real local man pages and
no response cache), grades every answer, and writes a report.

## Running

```sh
set -a; . ./.env; set +a               # API keys; never commit or print them

# 1. Sanity-check the dataset (no model calls): every reference must pass its
#    own checks, and with --exec, run reference[0] in the sandbox.
go run ./cmd/heyman-eval --self-test --exec

# 2. Evaluate models
go run ./cmd/heyman-eval --models anthropic/claude-haiku-4-5,ollama/ministral-3:3b --exec
go run ./cmd/heyman-eval --models anthropic/claude-haiku-4-5 --exec --judge anthropic/claude-sonnet-5-5
go run ./cmd/heyman-eval --models claude-code/haiku --filter bsd-gnu

make eval-self-test
make eval  EVAL_MODELS=anthropic/claude-sonnet-5-5 EVAL_ARGS="--judge anthropic/claude-sonnet-5-5"
make eval-quick                         # easy cases, no exec
```

| flag | meaning |
|---|---|
| `--cases FILE` | case file (default `evals/cases.yaml`) |
| `--models a,b` | `provider/model` specs (default `anthropic/claude-haiku-4-5`); any provider heyman supports, including `ollama/…` and `claude-code/…` |
| `--filter RE` | keep cases whose id **or any tag** matches (e.g. `bsd-gnu`, `^hard$`, `^git-`) |
| `--parallel N` | concurrent API attempts (default 4). Ollama models always run one at a time in their own lane. |
| `--repeat K` | run every case K times to measure variance (matrix cells become `k/K`) |
| `--timeout D` | per-attempt model timeout (default 3m) |
| `--exec` | run execution checks in a sandbox (see below) |
| `--judge SPEC` | also grade every answer with an LLM judge (reported separately, never changes `pass`) |
| `--max-cost USD` | stop scheduling new attempts before the estimated spend (models + judge) would exceed this |
| `--out DIR` | output directory (default `evals/results/<UTC timestamp>/`, gitignored) |
| `--self-test` | grade the references instead of calling models |
| `--list` | print the matching cases |

Ctrl-C stops scheduling, drops the in-flight attempts, and still writes the
report for everything that finished.

## Output

Each run directory has:

- `results.jsonl`: one line per (model, case, repeat), written as attempts finish. It holds the
  command produced, any error, every check with pass/fail, the exec outcome (including both
  stdouts), the judge's verdict and reason, latency, steps, tool calls, man pages read, tokens,
  and cost.
- `summary.md`: the same report that is printed to stdout.
- `run.json`: flags, OS, git HEAD, how long it took, estimated spend, and whether it was interrupted or hit the budget.

Some quick `jq` queries:

```sh
jq -r 'select(.pass|not) | [.model,.case_id,.command,.grade.first_failure] | @tsv' evals/results/*/results.jsonl
jq -r 'select(.judge.verdict and ((.judge.verdict=="correct") != .pass)) | [.case_id,.command,.judge.reason] | @tsv' …
```

## Reading the report

- **Summary.** Shows the pass rate (with n) overall, by difficulty, for `bsd-gnu` and `multi-tool`, and by
  form. *cmd form* is `heyman <command> …`, where that page is preloaded. *-- form* is `heyman -- …`, where
  the model picks its own pages. *errors* counts provider errors and runs where no command came back. Those
  count as failures.
- **Second table.** Latency (avg/p50/p95 of the model call), average tool calls (not counting the final
  `answer`), and *read extra man*: the share of attempts that read any man page beyond the preloaded one.
  For the `--` form, that means any page at all. It also shows cost per model and per case. Cost comes from
  the provider when it reports one (claude-code), otherwise from `internal/pricing`, and is $0 for ollama.
  `≥` means some attempts had no price. *exec pass* is passed/ran: exec runs that were skipped, or whose
  reference failed, don't count. *judge ok* is the judge's own pass rate, and *judge≠checks* counts the
  disagreements.
- **Per-case matrix.** `✓`/`✗`/`E` (provider error)/`–` (skipped). A `*` means the judge disagreed with the checks.
- **Failures.** For each failed attempt: the question, the candidate, `reference[0]`, the first failing
  check, the exec result, the judge's reasoning, and the man pages that were read.
- **Checks vs judge disagreements.** This is the place to find bad cases. If the checks fail and the judge
  says correct, the check is often too strict or a valid answer is missing from the references. If the checks
  pass and the judge says incorrect, the check is often too loose (or the judge is wrong). Look at each one
  before you change a case.

## How grading works

A candidate is first normalized: code fences, surrounding backticks and a leading `$ ` are removed, and
backslash-newline continuations are joined. Then:

1. **Deterministic checks.** All of `checks` must pass, plus `platforms[GOOS]`, and no `forbid` regex may
   match. `program: P` is true when P is *invoked* somewhere. The parser in `internal/eval/shell.go` splits on
   `| || && ; &`, newlines, subshells and groups, and respects quoting. It looks inside `$( )`, backticks,
   `<( )` and `sh -c '…'`. It skips `VAR=x` prefixes, and it looks past wrappers (`sudo`, `env`, `xargs [opts]`,
   `nohup`, `time`, `timeout`, `nice`, `command`, `exec`, `watch`) and into `find -exec … ;`. Placeholders such
   as `<file>` are treated as ordinary words.
2. **Exec** (with `--exec`, for cases that have an `exec:` block). The runner builds the fixture with `setup`
   in a fresh directory and runs `reference[0]`. It then rebuilds the fixture at the *same path* and runs the
   candidate, so absolute paths match. Both run with `bash -c`, stdin `/dev/null`, a clean env (`HOME` = the
   fixture, `LC_ALL=C TZ=UTC PAGER=cat GIT_PAGER=cat`), and a timeout that kills the whole process group.
   The candidate must exit 0. For `stdout` and `stdout_sorted`, its normalized stdout must equal the
   reference's: each line is trimmed, whitespace runs collapse, one leading `./` is stripped, and blank lines
   are dropped. Commands run under `sandbox-exec` on macOS, which denies the network and any writes outside the
   fixture and `/dev`, or under `bwrap` on Linux. Exec is **skipped** when no sandbox exists or the candidate
   contains a placeholder or `sudo`. When `reference[0]` itself fails or prints nothing, the result is
   `ref_error`: the case is broken and the attempt is not counted against the model.
3. **Judge** (with `--judge`). The judge model gets the question, the OS (with the macOS version), the
   references (`references_by_os[GOOS]` when present) and the candidate. It answers with
   `{"verdict":"correct|incorrect","reason":…}`. `claude-code/…` judges run `claude -p --tools ""` in an empty
   temp dir. No sampling parameters are set.

`pass` = deterministic pass **and** exec pass (when exec ran). The judge is recorded separately.

Cases tagged `macos-only` are skipped on other OSes, and `needs-jq` cases are skipped when `jq` is missing.
Skips show as `–` and are left out of pass rates.

## Adding a case

1. Add an entry to `evals/cases.yaml`. The header comment there documents every field. Unknown keys are
   errors. Every case needs a unique kebab-case `id`, exactly one of `easy|medium|hard` in `tags`, at least
   one `reference` and at least one check.
2. Keep checks about *behaviour*, not spelling. Use `any:` for alternatives such as different tools or
   equivalent flags. Put OS-specific flag requirements under `platforms:`, and tag the case `bsd-gnu` when
   macOS and Linux need different flags. Add the GNU form under `references_by_os: {linux: [...]}`.
3. If the answer can be checked by running it, add an `exec:` block. `setup` must be portable bash that only
   writes inside the current directory, and `reference[0]` must be portable too.
4. Run `go run ./cmd/heyman-eval --self-test --exec --filter '^your-id$'`. It must report 0 errors.
5. Run one cheap model with `--judge` on the case, and read any disagreement.

## Known limitations

- `date-yesterday` exec can flake when a run crosses midnight UTC.
- Some answers pass the regex checks but intentionally fail exec. For example, `find … -exec wc -c {} +`
  prints an extra "total" line.
- Exec compares against `reference[0]` only, so a correct answer whose output is formatted differently
  fails exec. The judge column helps spot these.
- The pricing table has no cache-token prices. Cache reads are estimated at 0.1× the input price and cache
  writes at 1.25×.
