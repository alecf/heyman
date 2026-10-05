package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"

	"github.com/alecf/heyman/internal/assist"
	"github.com/alecf/heyman/internal/cache"
	"github.com/alecf/heyman/internal/config"
	"github.com/alecf/heyman/internal/llm"
	"github.com/alecf/heyman/internal/manpage"
	"github.com/alecf/heyman/internal/output"
	"github.com/alecf/heyman/internal/pricing"
	"github.com/alecf/heyman/internal/spinner"
	"github.com/spf13/cobra"
)

type rootFlags struct {
	model   string
	section string
	profile string
	noCache bool
	verbose bool
	debug   bool
	dryRun  bool
	quiet   bool
	explain bool
	json    bool
	tokens  bool
	copy    bool
}

// Execute runs the heyman CLI.
func Execute(version, commit, date string) error {
	return newRootCmd(version, commit, date, run).Execute()
}

// newRootCmd builds the command tree; runFn handles the root command (tests
// substitute it to inspect argument parsing).
func newRootCmd(version, commit, date string, runFn func(*cobra.Command, *rootFlags, []string) error) *cobra.Command {
	var f rootFlags

	rootCmd := &cobra.Command{
		Use:   "heyman [flags] <command> <request>\n  heyman [flags] -- <request>",
		Short: "Ask for a shell command in plain English",
		Long: `heyman turns a plain-English request into a shell command, checking the
man pages on this machine so the flags match your system.

Name the program you have in mind, or use -- and let heyman pick:
  heyman lsof which process is listening on port 8080
  heyman -- list the size of all files changed in git in the last week

Choose a model with --model provider/model (default ` + llm.DefaultModel + `):
  providers: ` + strings.Join(llm.Providers(), ", "),
		Version:       fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, date),
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFn(cmd, &f, args)
		},
	}

	pf := rootCmd.PersistentFlags()
	pf.StringVarP(&f.model, "model", "m", "", "model as provider/model, e.g. anthropic/claude-haiku-4-5 or ollama/qwen3:4b (env HEYMAN_MODEL)")
	pf.StringVarP(&f.profile, "profile", "p", "", "named profile from the config file (env HEYMAN_PROFILE)")
	pf.BoolVar(&f.noCache, "no-cache", false, "bypass cache for this query")
	pf.BoolVarP(&f.verbose, "verbose", "v", false, "show model, man pages consulted and tool calls on stderr")
	pf.BoolVarP(&f.quiet, "quiet", "q", false, "suppress progress messages")
	pf.BoolVarP(&f.debug, "debug", "d", false, "also print the system prompt and full tool trace on stderr")
	pf.BoolVar(&f.dryRun, "dry-run", false, "print the prompt (as sent to a tool-calling model) without calling a model")

	fl := rootCmd.Flags()
	fl.StringVarP(&f.section, "section", "s", "", "man page section of <command>, e.g. 3 for `heyman -s 3 printf …`")
	fl.BoolVarP(&f.explain, "explain", "e", false, "include an explanation")
	fl.BoolVarP(&f.json, "json", "j", false, "JSON output with metadata")
	fl.BoolVarP(&f.tokens, "tokens", "t", false, "show token usage and costs")
	fl.BoolVarP(&f.copy, "copy", "c", false, "copy command to clipboard")

	// Stop flag parsing at the first positional argument so requests like
	// `heyman ls -la what does this do` don't have their words parsed as flags.
	fl.SetInterspersed(false)

	rootCmd.AddCommand(profileCmd())
	rootCmd.AddCommand(testConfigCmd())
	rootCmd.AddCommand(cacheStatsCmd())
	rootCmd.AddCommand(clearCacheCmd())

	return rootCmd
}

// parseRequest splits argv into a Request. dash is cobra's ArgsLenAtDash
// and section the --section flag. Forms:
//
//	heyman <command> <words…>          command named, its man page preloaded
//	heyman <section> <command> <words…>
//	heyman -s <section> <command> <words…>
//	heyman <command> -- <words…>        words may start with dashes
//	heyman -- <words…>                  no command; the model chooses
//	heyman "<a whole request>"          a single quoted argument also means no command
func parseRequest(args []string, dash int, section string) (assist.Request, error) {
	var req assist.Request
	if len(args) == 0 {
		return req, errNoRequest
	}
	if dash == 0 || (dash < 0 && len(args) == 1 && strings.ContainsAny(strings.TrimSpace(args[0]), " \t")) {
		req.Question = strings.TrimSpace(strings.Join(args, " "))
		if req.Question == "" {
			return req, errNoRequest
		}
		return req, nil
	}
	if dash < 0 {
		// Root flags stop at the first positional argument, so a `--` after
		// the command reaches us as a literal argument: `heyman git -- --since
		// what` or `heyman 3 printf -- …`. Treat it as the separator.
		i := 1
		if isSectionArg(args[0]) {
			i = 2
		}
		if i < len(args) && args[i] == "--" {
			args = append(append([]string(nil), args[:i]...), args[i+1:]...)
		}
	} else {
		args = append([]string(nil), args...) // `--` already removed by cobra
	}
	cmd, sec, q := manpage.ParseCommand(args)
	req.Command, req.Section = cmd, sec
	if section != "" {
		req.Section = section
	}
	req.Question = strings.TrimSpace(strings.Join(q, " "))
	if req.Command == "" {
		return req, errNoRequest
	}
	if req.Question == "" {
		return req, fmt.Errorf("no request given for %q. Usage: heyman %s <what you want to do>  (or: heyman -- %s)", req.Command, req.Command, req.Command)
	}
	return req, nil
}

var errNoRequest = errors.New("no request given. Usage: heyman <command> <what you want to do>, or heyman -- <what you want to do>")

func isSectionArg(s string) bool {
	return len(s) == 1 && s[0] >= '1' && s[0] <= '9'
}

// resolveModel picks the model, most explicit first: --model, --profile,
// HEYMAN_MODEL, HEYMAN_PROFILE, the config's default_profile, then
// llm.DefaultModel. source says which one won (for -v and errors).
func resolveModel(f *rootFlags, cfg *config.Config) (model, baseURL, source string, err error) {
	if f.model != "" {
		return f.model, "", "--model", nil
	}
	name, from := f.profile, "--profile"
	if name == "" {
		if m := os.Getenv("HEYMAN_MODEL"); m != "" {
			return m, "", "HEYMAN_MODEL", nil
		}
		name, from = cfg.ActiveProfile()
	}
	if name == "" {
		return llm.DefaultModel, "", "default", nil
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		avail := "none; run: heyman profile setup"
		if names := cfg.SortedProfileNames(); len(names) > 0 {
			avail = strings.Join(names, ", ")
		}
		if from == "--profile" || from == "HEYMAN_PROFILE" {
			return "", "", "", fmt.Errorf("profile %q (from %s) not found (available: %s)", name, from, avail)
		}
		// Don't silently fall back to a paid default model: say how to fix it.
		return "", "", "", fmt.Errorf("profile %q (from %s) not found (available: %s).\nFix it with: heyman profile set-default <name>, or choose one per run with --profile or --model", name, from, avail)
	}
	if err := p.Validate(); err != nil {
		return "", "", "", fmt.Errorf("%w (in %s)", err, config.Path())
	}
	return p.Spec(), p.BaseURL, "profile " + name, nil
}

func run(cmd *cobra.Command, f *rootFlags, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	// Ctrl-C cancels the model call (and the claude subprocess) cleanly so
	// the spinner line gets cleared.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	req, err := parseRequest(args, cmd.ArgsLenAtDash(), f.section)
	if err != nil {
		if errors.Is(err, errNoRequest) && len(args) == 0 {
			_ = cmd.Usage()
		}
		return err
	}
	req.Explain = f.explain

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	model, baseURL, source, err := resolveModel(f, cfg)
	if err != nil {
		return err
	}
	spec, err := llm.ParseSpec(model)
	if err != nil {
		return err
	}

	logf := func(format string, a ...any) {
		if f.verbose || f.debug {
			fmt.Fprintf(os.Stderr, format+"\n", a...)
		}
	}
	logf("Model: %s (%s)", spec, source)
	if baseURL != "" {
		logf("Base URL: %s", baseURL)
	}
	if req.Command != "" {
		logf("Command: %s", pageKeyFor(req))
	}
	logf("Request: %s", req.Question)

	man := manpage.NewFetcher()

	if f.dryRun || f.debug {
		system, user, err := assist.PromptPreview(man, req)
		if err != nil {
			return err
		}
		if f.dryRun {
			fmt.Printf("=== System prompt ===\n%s\n\n=== User prompt ===\n%s\n", system, user)
			return nil
		}
		fmt.Fprintf(os.Stderr, "=== System prompt ===\n%s\n=== User prompt ===\n%s\n=====================\n", system, user)
	}

	mode := "command"
	if req.Explain {
		mode = "explain"
	}
	key := cache.GenerateKey(spec.String(), baseURL, mode, req.Command, req.Section, req.Question, runtime.GOOS)
	cacheManager := cache.New(cfg.CacheDays)

	var res assist.Result
	if !f.noCache && cacheManager.Get(key, &res) {
		res.Cached = true
		logf("Served from cache")
	} else {
		var spin *spinner.Spinner
		if !f.quiet && !f.verbose && !f.debug {
			spin = spinner.New(fmt.Sprintf("Asking %s…", spec))
			spin.Start()
		}
		stopSpin := func() {
			if spin != nil {
				spin.Stop()
			}
		}
		onEvent := func(e assist.Event) {
			switch {
			case f.verbose || f.debug:
				if e.Kind == "note" {
					fmt.Fprintf(os.Stderr, "  note: %s\n", e.Detail)
				} else {
					fmt.Fprintf(os.Stderr, "  → %s %s\n", e.Tool, e.Detail)
				}
			case spin != nil && e.Tool == "man":
				spin.Update(fmt.Sprintf("Reading man %s…", e.Detail))
			case spin != nil && e.Tool == "man_search":
				spin.Update(fmt.Sprintf("Searching man pages for %q…", e.Detail))
			}
		}
		answerer, _, err := assist.New(ctx, assist.Config{Model: spec.String(), BaseURL: baseURL, Man: man, OnEvent: onEvent})
		if err != nil {
			stopSpin()
			return err
		}
		out, err := answerer.Ask(ctx, req)
		stopSpin()
		if f.debug && out != nil {
			for _, tc := range out.ToolCalls {
				fmt.Fprintf(os.Stderr, "  tool %s %s %s\n", tc.Tool, tc.Input, tc.Error)
			}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted")
		}
		if errors.Is(err, assist.ErrNoCommand) {
			return fmt.Errorf("%s did not produce a command; try rephrasing, or a different --model", spec)
		}
		if err != nil {
			return err
		}
		res = *out
		if err := cacheManager.Set(key, req.Command, req.Question, spec.String(), &res); err != nil {
			logf("Warning: failed to cache response: %v", err)
		}
	}

	if len(res.ManPages) > 0 {
		logf("Man pages consulted: %s", strings.Join(res.ManPages, ", "))
	}
	logf("Steps: %d, tokens in/out: %d/%d", res.Steps, res.Usage.InputTokens, res.Usage.OutputTokens)

	return outputResult(f, spec, &res, req.Explain)
}

func pageKeyFor(req assist.Request) string {
	if req.Section != "" {
		return fmt.Sprintf("%s(%s)", req.Command, req.Section)
	}
	return req.Command
}

func outputResult(f *rootFlags, spec llm.Spec, res *assist.Result, explain bool) error {
	db := pricing.GetDatabase()
	mp := db.GetPricing(spec.String())
	cost := res.CostUSD
	if cost == nil && mp != nil {
		c := mp.CalculateCost(res.Usage.InputTokens, res.Usage.OutputTokens)
		cost = &c
	}
	if res.Cached {
		// Nothing was spent on this run.
		zero := 0.0
		cost = &zero
	}

	if f.json {
		jsonOutput, err := output.FormatJSON(res, explain, cost)
		if err != nil {
			return err
		}
		fmt.Println(jsonOutput)
	} else {
		fmt.Println(res.Command)
		if explain && res.Explanation != "" {
			fmt.Println()
			fmt.Println(res.Explanation)
		}
		if f.tokens {
			fmt.Fprintln(os.Stderr)
			if res.Cached {
				fmt.Fprintln(os.Stderr, "Token usage: none (served from cache; use --no-cache to ask again)")
			} else {
				fmt.Fprintln(os.Stderr, pricing.FormatTokenUsage(res.Usage.InputTokens, res.Usage.OutputTokens, cost, pricing.IsFree(spec.Provider), mp, db.LastUpdated))
			}
		}
	}

	if f.copy {
		if err := output.CopyToClipboard(res.Command); err != nil {
			return fmt.Errorf("failed to copy to clipboard: %w", err)
		}
		if !f.json && !f.quiet {
			fmt.Fprintln(os.Stderr, "✓ Copied to clipboard")
		}
	}
	return nil
}
