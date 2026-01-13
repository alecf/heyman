package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/alecf/heyman/internal/cache"
	"github.com/alecf/heyman/internal/config"
	"github.com/alecf/heyman/internal/llm"
	"github.com/alecf/heyman/internal/manpage"
	"github.com/alecf/heyman/internal/output"
	"github.com/alecf/heyman/internal/parser"
	"github.com/alecf/heyman/internal/pricing"
	"github.com/alecf/heyman/internal/prompt"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile string
	profile string
	noCache bool
	verbose bool
	debug   bool
	dryRun  bool
	quiet   bool
)

func Execute(version, commit, date string) error {
	rootCmd := &cobra.Command{
		Use:   "heyman [flags] <command> <question>",
		Short: "LLM-powered man page Q&A",
		Long: `heyman wraps man pages with LLM-powered Q&A, enabling natural language
queries about command-line tools.

Example:
  heyman lsof how do I list the ports that a pid is listening on`,
		Version: fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, date),
		Args:    cobra.MinimumNArgs(2),
		RunE:    run,
	}

	// Global flags
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ~/.config/heyman/config.toml)")
	rootCmd.PersistentFlags().StringVarP(&profile, "profile", "p", "", "LLM profile to use")
	rootCmd.PersistentFlags().BoolVar(&noCache, "no-cache", false, "bypass cache for this query")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "show operation details")
	rootCmd.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false, "suppress progress messages")
	rootCmd.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "show full request/response details")
	rootCmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "show prompt without making API call")

	// Output mode flags
	rootCmd.Flags().BoolP("explain", "e", false, "include explanation (streaming)")
	rootCmd.Flags().BoolP("json", "j", false, "JSON output with metadata")
	rootCmd.Flags().BoolP("tokens", "t", false, "show token usage and costs")
	rootCmd.Flags().BoolP("copy", "c", false, "copy command to clipboard")

	// Management commands
	rootCmd.AddCommand(setupCmd())
	rootCmd.AddCommand(setProfileCmd())
	rootCmd.AddCommand(listProfilesCmd())
	rootCmd.AddCommand(testConfigCmd())
	rootCmd.AddCommand(cacheStatsCmd())
	rootCmd.AddCommand(clearCacheCmd())

	// Bind flags to viper
	viper.BindPFlag("profile", rootCmd.PersistentFlags().Lookup("profile"))
	viper.BindPFlag("no-cache", rootCmd.PersistentFlags().Lookup("no-cache"))
	viper.BindPFlag("verbose", rootCmd.PersistentFlags().Lookup("verbose"))
	viper.BindPFlag("debug", rootCmd.PersistentFlags().Lookup("debug"))

	// Environment variable support
	viper.SetEnvPrefix("HEYMAN")
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

	return rootCmd.Execute()
}

func run(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Parse command and question
	command, section, questionParts := manpage.ParseCommand(args)
	if command == "" {
		return fmt.Errorf("no command specified")
	}
	if len(questionParts) == 0 {
		return fmt.Errorf("no question specified")
	}
	question := strings.Join(questionParts, " ")

	// Get active profile
	activeProfile, err := cfg.GetActiveProfile()
	if err != nil {
		return fmt.Errorf("no profile configured: %w\nRun 'heyman setup' to configure", err)
	}

	if verbose {
		fmt.Printf("Command: %s\n", command)
		if section != "" {
			fmt.Printf("Section: %s\n", section)
		}
		fmt.Printf("Question: %s\n", question)
		fmt.Printf("Using profile: %s (%s %s)\n", activeProfile.Name, activeProfile.Provider, activeProfile.Model)
	}

	// Fetch man page
	fetcher := manpage.NewFetcher()
	manPageContent, err := fetcher.Fetch(command, section)
	if err != nil {
		return err
	}

	if verbose {
		fmt.Printf("Man page size: %d bytes\n", len(manPageContent))
	}

	// Create provider with context window detection
	providerConfig, err := CreateProvider(cmd.Context(), cfg, activeProfile, verbose)
	if err != nil {
		return err
	}

	// Build prompt
	explainFlag, _ := cmd.Flags().GetBool("explain")
	promptBuilder := prompt.NewBuilder(command, manPageContent, question, explainFlag)
	userPrompt := promptBuilder.UserPrompt()

	// Count tokens and warn if exceeds context window
	actualTokens := countTokens(userPrompt, verbose)
	if actualTokens > providerConfig.ContextWindow {
		fmt.Fprintf(os.Stderr, "⚠️  Warning: Prompt (%d tokens) exceeds %d token context window.\n", actualTokens, providerConfig.ContextWindow)
		fmt.Fprintf(os.Stderr, "    Try a more specific section: heyman <section> %s <question>\n\n", command)
	}

	// Debug output
	if debug {
		fmt.Fprintf(os.Stderr, "\n=== DEBUG: System Prompt ===\n%s\n", promptBuilder.SystemPrompt())
		fmt.Fprintf(os.Stderr, "\n=== DEBUG: User Prompt (first 500 chars) ===\n%s\n", truncate(userPrompt, 500))
		fmt.Fprintf(os.Stderr, "\n=== DEBUG: User Prompt length: %d chars ===\n\n", len(userPrompt))
	}

	// Query LLM (with caching)
	resp, err := queryWithCache(cmd, cfg, providerConfig, promptBuilder, activeProfile, command, question)
	if err != nil {
		return err
	}

	// Parse and validate response (with retry)
	parsed, err := parseAndValidate(cmd, providerConfig.Provider, promptBuilder, resp, command, explainFlag, cfg, activeProfile, question)
	if err != nil {
		return err
	}

	// Output result
	return outputResult(cmd, parsed, resp, activeProfile, cfg)
}

func queryWithCache(cmd *cobra.Command, cfg *config.Config, providerConfig *ProviderConfig, promptBuilder *prompt.Builder, activeProfile *config.Profile, command, question string) (*llm.QueryResponse, error) {
	cacheManager := cache.New(cfg.CacheDays)

	// Check cache first
	if !noCache {
		if cachedResp, found := cacheManager.Get(command, question, activeProfile.Model); found {
			if verbose {
				fmt.Println("Found in cache")
			}
			return cachedResp, nil
		}
	}

	// Prepare request
	req := llm.QueryRequest{
		Model:         activeProfile.Model,
		SystemPrompt:  promptBuilder.SystemPrompt(),
		UserPrompt:    promptBuilder.UserPrompt(),
		MaxTokens:     2000,
		Temperature:   0.1,
		ContextWindow: providerConfig.ContextWindow,
	}

	// Execute query
	showProgress := !quiet && !verbose && !debug
	resp, err := ExecuteQuery(cmd.Context(), providerConfig.Provider, req, QueryOptions{
		ShowProgress: showProgress,
		Verbose:      verbose,
		Debug:        debug,
		Profile:      activeProfile,
	})
	if err != nil {
		return nil, err
	}

	// Save to cache
	if err := cacheManager.Set(command, question, activeProfile.Model, resp); err != nil {
		if verbose {
			fmt.Printf("Warning: failed to cache response: %v\n", err)
		}
	}

	return resp, nil
}

func parseAndValidate(cmd *cobra.Command, provider llm.Provider, promptBuilder *prompt.Builder, resp *llm.QueryResponse, command string, explainFlag bool, cfg *config.Config, activeProfile *config.Profile, question string) (parser.ParsedResponse, error) {
	responseParser := parser.New(command, explainFlag)
	parsed := responseParser.Parse(resp.Content)

	// Retry if invalid (and not from cache)
	if !parsed.Valid && !resp.Cached {
		if verbose {
			fmt.Printf("Validation failed: %v, retrying with strict prompt\n", parsed.Error)
		}

		req := llm.QueryRequest{
			Model:        activeProfile.Model,
			SystemPrompt: promptBuilder.SystemPrompt(),
			UserPrompt:   promptBuilder.StrictRetryPrompt(),
			MaxTokens:    2000,
			Temperature:  0.1,
		}

		retryResp, err := provider.Query(cmd.Context(), req)
		if err != nil {
			return parser.ParsedResponse{}, fmt.Errorf("LLM retry failed: %w", err)
		}

		parsed = responseParser.Parse(retryResp.Content)
		if !parsed.Valid {
			return parser.ParsedResponse{}, fmt.Errorf("unable to generate valid command: %v", parsed.Error)
		}

		// Cache successful retry
		cacheManager := cache.New(cfg.CacheDays)
		if err := cacheManager.Set(command, question, activeProfile.Model, retryResp); err != nil {
			if verbose {
				fmt.Printf("Warning: failed to cache retry response: %v\n", err)
			}
		}
	} else if !parsed.Valid && resp.Cached {
		return parser.ParsedResponse{}, fmt.Errorf("cached response invalid: %v", parsed.Error)
	}

	return parsed, nil
}

func outputResult(cmd *cobra.Command, parsed parser.ParsedResponse, resp *llm.QueryResponse, activeProfile *config.Profile, cfg *config.Config) error {
	jsonFlag, _ := cmd.Flags().GetBool("json")
	tokensFlag, _ := cmd.Flags().GetBool("tokens")
	copyFlag, _ := cmd.Flags().GetBool("copy")
	explainFlag, _ := cmd.Flags().GetBool("explain")

	// Calculate cost if needed
	var costPtr *float64
	if jsonFlag || tokensFlag {
		pricingDB := pricing.GetDatabase()
		if modelPricing := pricingDB.GetPricing(activeProfile.Model); modelPricing != nil {
			cost := modelPricing.CalculateCost(resp.TokensInput, resp.TokensOutput)
			costPtr = &cost
		}
	}

	// Output based on format
	if jsonFlag {
		jsonOutput, err := output.FormatJSON(parsed, resp, costPtr)
		if err != nil {
			return fmt.Errorf("failed to format JSON: %w", err)
		}
		fmt.Println(jsonOutput)
	} else {
		// Plain text output
		fmt.Println(parsed.Command)
		if explainFlag && parsed.Explanation != "" {
			fmt.Println()
			fmt.Println(parsed.Explanation)
		}

		// Show token usage if requested
		if tokensFlag {
			fmt.Println()
			pricingDB := pricing.GetDatabase()
			modelPricing := pricingDB.GetPricing(activeProfile.Model)
			tokenInfo := pricing.FormatTokenUsage(resp.TokensInput, resp.TokensOutput, modelPricing, pricingDB.LastUpdated)
			fmt.Println(tokenInfo)
		}
	}

	// Copy to clipboard if requested
	if copyFlag {
		if err := output.CopyToClipboard(parsed.Command); err != nil {
			return fmt.Errorf("failed to copy to clipboard: %w", err)
		}
		if !jsonFlag {
			fmt.Println("\n✓ Copied to clipboard")
		}
	}

	return nil
}
