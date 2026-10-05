package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
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

func Execute(version, commit, date string) error {
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
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, &f, args)
		},
	}

	pf := rootCmd.PersistentFlags()
	pf.StringVarP(&f.model, "model", "m", "", "model as provider/model, e.g. anthropic/claude-haiku-4-5 or ollama/qwen3:4b (env HEYMAN_MODEL)")
	pf.StringVarP(&f.profile, "profile", "p", "", "named profile from the config file (env HEYMAN_PROFILE)")
	pf.BoolVar(&f.noCache, "no-cache", false, "bypass cache for this query")
	pf.BoolVarP(&f.verbose, "verbose", "v", false, "show model, man pages consulted and tool calls on stderr")
	pf.BoolVarP(&f.quiet, "quiet", "q", false, "suppress progress messages")
	pf.BoolVarP(&f.debug, "debug", "d", false, "show the full prompt and tool trace on stderr")
	pf.BoolVar(&f.dryRun, "dry-run", false, "print the prompt without calling a model")

	fl := rootCmd.Flags()
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

	return rootCmd.Execute()
}

// parseRequest splits argv into a Request. Forms:
//
//	heyman <command> <words…>          command named, its man page preloaded
//	heyman <section> <command> <words…>
//	heyman -- <words…>                  no command; the model chooses
//	heyman "<a whole request>"          a single quoted argument also means no command
func parseRequest(args []string, dash int) (assist.Request, error) {
	var req assist.Request
	if dash == 0 || (dash < 0 && len(args) == 1 && strings.ContainsAny(args[0], " \t")) {
		req.Question = strings.TrimSpace(strings.Join(args, " "))
		if req.Question == "" {
			return req, fmt.Errorf("no request given")
		}
		return req, nil
	}
	if dash > 0 {
		// `heyman git -- --since what` : everything after -- belongs to the question.
		before, after := args[:dash], args[dash:]
		cmd, section, q := manpage.ParseCommand(before)
		req.Command, req.Section = cmd, section
		req.Question = strings.Join(append(q, after...), " ")
	} else {
		cmd, section, q := manpage.ParseCommand(args)
		req.Command, req.Section = cmd, section
		req.Question = strings.Join(q, " ")
	}
	if req.Command == "" {
		return req, fmt.Errorf("no command specified")
	}
	if strings.TrimSpace(req.Question) == "" {
		return req, fmt.Errorf("no request given for %q. Usage: heyman %s <what you want to do>", req.Command, req.Command)
	}
	return req, nil
}

// resolveModel picks the model: --model, HEYMAN_MODEL, --profile/HEYMAN_PROFILE,
// the config's default profile, then llm.DefaultModel.
func resolveModel(f *rootFlags, cfg *config.Config) (model, baseURL, source string, err error) {
	if f.model != "" {
		return f.model, "", "--model", nil
	}
	if m := os.Getenv("HEYMAN_MODEL"); m != "" {
		return m, "", "HEYMAN_MODEL", nil
	}
	name := f.profile
	if name == "" {
		name = cfg.DefaultProfile // includes HEYMAN_PROFILE, applied by config.Load
	}
	if name != "" {
		p, ok := cfg.Profiles[name]
		if !ok {
			return "", "", "", fmt.Errorf("profile %q not found (available: %s)", name, strings.Join(cfg.SortedProfileNames(), ", "))
		}
		return p.Spec(), p.BaseURL, "profile " + name, nil
	}
	return llm.DefaultModel, "", "default", nil
}

func run(cmd *cobra.Command, f *rootFlags, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	req, err := parseRequest(args, cmd.ArgsLenAtDash())
	if err != nil {
		return err
	}
	req.Explain = f.explain

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
	if req.Command != "" {
		logf("Command: %s", pageKeyFor(req))
	}
	logf("Request: %s", req.Question)

	man := manpage.NewFetcher()

	if f.dryRun {
		system, user, err := assist.PromptPreview(man, req)
		if err != nil {
			return err
		}
		fmt.Printf("=== System prompt ===\n%s\n\n=== User prompt ===\n%s\n", system, user)
		return nil
	}

	mode := "command"
	if req.Explain {
		mode = "explain"
	}
	key := cache.GenerateKey(spec.String(), mode, req.Command, req.Section, req.Question, runtime.GOOS)
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
		onEvent := func(e assist.Event) {
			switch {
			case f.verbose || f.debug:
				fmt.Fprintf(os.Stderr, "  → %s %s\n", e.Tool, e.Detail)
			case spin != nil && e.Tool == "man":
				spin.Update(fmt.Sprintf("Reading man %s…", e.Detail))
			case spin != nil && e.Tool == "man_search":
				spin.Update(fmt.Sprintf("Searching man pages for %q…", e.Detail))
			}
		}
		answerer, _, err := assist.New(ctx, assist.Config{Model: spec.String(), BaseURL: baseURL, Man: man, OnEvent: onEvent})
		if err != nil {
			if spin != nil {
				spin.Stop()
			}
			return err
		}
		out, err := answerer.Ask(ctx, req)
		if spin != nil {
			spin.Stop()
		}
		if f.debug && out != nil {
			for _, tc := range out.ToolCalls {
				fmt.Fprintf(os.Stderr, "  tool %s %s %s\n", tc.Tool, tc.Input, tc.Error)
			}
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
			fmt.Fprintln(os.Stderr, pricing.FormatTokenUsage(res.Usage.InputTokens, res.Usage.OutputTokens, cost, pricing.IsFree(spec.Provider), mp, db.LastUpdated))
		}
	}

	if f.copy {
		if err := output.CopyToClipboard(res.Command); err != nil {
			return fmt.Errorf("failed to copy to clipboard: %w", err)
		}
		if !f.json {
			fmt.Fprintln(os.Stderr, "✓ Copied to clipboard")
		}
	}
	return nil
}
