package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/alecf/heyman/internal/cache"
	"github.com/alecf/heyman/internal/config"
	"github.com/alecf/heyman/internal/llm"
	"github.com/spf13/cobra"
)

// validModelName checks if a model name is safe for use in profile names
// Allows: alphanumeric, dots, colons, slashes (OpenRouter ids), hyphens, underscores
var validModelName = regexp.MustCompile(`^[a-zA-Z0-9._:/-]+$`)

// sizePattern matches size indicators like 1b, 7b, 70b, 270b
var sizePattern = regexp.MustCompile(`(?i)^(\d+[bB])`)

var (
	unsafeProfileChars = regexp.MustCompile(`[^a-z0-9_-]`)
	hyphenRuns         = regexp.MustCompile(`-+`)
)

// sanitizeProfileName creates a safe, lowercase profile name from user input
func sanitizeProfileName(input string) string {
	safe := unsafeProfileChars.ReplaceAllString(strings.ToLower(input), "-")
	safe = hyphenRuns.ReplaceAllString(safe, "-")
	return strings.Trim(safe, "-")
}

// extractSizeSuffix extracts a meaningful size suffix from a model tag
// e.g., "1b-instruct-q4_0" -> "1b", "latest" -> ""
func extractSizeSuffix(tag string) string {
	// Skip noise tags
	noise := []string{"latest", "instruct", "chat", "base"}
	for _, n := range noise {
		if strings.EqualFold(tag, n) {
			return ""
		}
	}

	// Check for size pattern at the start
	if match := sizePattern.FindStringSubmatch(tag); len(match) > 1 {
		return strings.ToLower(match[1])
	}

	// Check parts separated by hyphens
	parts := strings.Split(tag, "-")
	for _, part := range parts {
		// Skip quantization patterns like q4_0, q8
		if strings.HasPrefix(strings.ToLower(part), "q") {
			continue
		}
		if match := sizePattern.FindStringSubmatch(part); len(match) > 1 {
			return strings.ToLower(match[1])
		}
	}

	return ""
}

// generateProfileName creates a unique profile name with collision avoidance
// Priority: base name -> base-size -> provider-base-size
func generateProfileName(cfg *config.Config, provider, model string) string {
	var baseName, tag string

	// Split model into base and tag (for Ollama-style names)
	baseName, tag, _ = strings.Cut(model, ":")
	// OpenRouter ids and Hugging Face paths ("moonshotai/kimi-k2",
	// "hf.co/unsloth/Qwen3-4B-GGUF"): the last path element names the model.
	if i := strings.LastIndex(baseName, "/"); i >= 0 && i < len(baseName)-1 {
		baseName = baseName[i+1:]
	}

	sanitizedBase := sanitizeProfileName(baseName)
	if sanitizedBase == "" {
		sanitizedBase = sanitizeProfileName(provider)
	}
	if sanitizedBase == "" {
		sanitizedBase = "profile"
	}
	sizeSuffix := extractSizeSuffix(tag)

	// Try just the base name first
	candidate := sanitizedBase
	if !cfg.ProfileExists(candidate) {
		return candidate
	}

	// Try base-size if we have a size suffix
	if sizeSuffix != "" {
		candidate = fmt.Sprintf("%s-%s", sanitizedBase, sizeSuffix)
		if !cfg.ProfileExists(candidate) {
			return candidate
		}
	}

	// Try provider-base
	candidate = fmt.Sprintf("%s-%s", provider, sanitizedBase)
	if !cfg.ProfileExists(candidate) {
		return candidate
	}

	// Try provider-base-size
	if sizeSuffix != "" {
		candidate = fmt.Sprintf("%s-%s-%s", provider, sanitizedBase, sizeSuffix)
		if !cfg.ProfileExists(candidate) {
			return candidate
		}
	}

	// Last resort: append numbers
	for i := 2; ; i++ {
		candidate = fmt.Sprintf("%s-%d", sanitizedBase, i)
		if !cfg.ProfileExists(candidate) {
			return candidate
		}
	}
}

// profileCmd creates the main profile command group
func profileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Manage LLM profiles",
		Long: `Manage LLM profiles for heyman.

Subcommands:
  list         List all configured profiles
  setup        Interactive wizard to create a new profile
  delete       Delete a profile
  set-default  Set the default profile
  show         Show details of a profile`,
	}

	cmd.AddCommand(profileListCmd())
	cmd.AddCommand(profileSetupCmd())
	cmd.AddCommand(profileDeleteCmd())
	cmd.AddCommand(profileSetDefaultCmd())
	cmd.AddCommand(profileShowCmd())

	return cmd
}

func profileListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all configured profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			if len(cfg.Profiles) == 0 {
				fmt.Println("No profiles configured. Run 'heyman profile setup' to create one.")
				return nil
			}
			if cfg.DefaultProfile != "" && !cfg.ProfileExists(cfg.DefaultProfile) {
				fmt.Fprintf(os.Stderr, "Warning: default_profile %q doesn't match any profile; fix with: heyman profile set-default <name>\n", cfg.DefaultProfile)
			}
			active, _ := cfg.ActiveProfile()

			// Find max lengths for alignment
			maxName := 0
			maxProvider := 0
			for name, profile := range cfg.Profiles {
				if len(name) > maxName {
					maxName = len(name)
				}
				if len(profile.Provider) > maxProvider {
					maxProvider = len(profile.Provider)
				}
			}

			for _, name := range cfg.SortedProfileNames() {
				profile := cfg.Profiles[name]
				marker := " "
				if name == active {
					marker = "*"
				}
				fmt.Printf("%s %-*s  %-*s  %s\n", marker, maxName, name, maxProvider, profile.Provider, profile.Model)
			}

			return nil
		},
	}
}

func profileSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Interactive wizard to create a new profile",
		Long: `Interactive setup wizard to configure a new heyman profile.

You can also set up profiles manually by editing:
  ~/.config/heyman/config.toml (Linux/others)
  ~/Library/Application Support/heyman/config.toml (macOS)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Heyman Profile Setup")
			fmt.Println("====================")
			fmt.Println()

			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			reader := bufio.NewReader(os.Stdin)
			var inputErr error
			ask := func(prompt, def string) string {
				if inputErr != nil {
					return ""
				}
				if def != "" {
					fmt.Printf("%s [%s]: ", prompt, def)
				} else {
					fmt.Printf("%s: ", prompt)
				}
				line, err := reader.ReadString('\n')
				if err != nil && line == "" {
					// EOF: don't silently create a profile from defaults.
					inputErr = fmt.Errorf("setup needs interactive input (stdin closed)")
					fmt.Println()
					return ""
				}
				line = strings.TrimSpace(line)
				if line == "" {
					return def
				}
				return line
			}

			fmt.Println("Select a provider:")
			for i, p := range setupProviders {
				fmt.Printf("  %d) %-14s %s\n", i+1, p.name, p.blurb)
			}
			choice := ask(fmt.Sprintf("\nChoice [1-%d]", len(setupProviders)), "1")
			if inputErr != nil {
				return inputErr
			}
			idx := 0
			if _, err := fmt.Sscanf(choice, "%d", &idx); err != nil || idx < 1 || idx > len(setupProviders) {
				return fmt.Errorf("invalid choice %q", choice)
			}
			sp := setupProviders[idx-1]

			model := ask("Model", sp.defaultModel)
			if inputErr != nil {
				return inputErr
			}
			if model == "" {
				return fmt.Errorf("a model name is required")
			}
			if !validModelName.MatchString(model) {
				return fmt.Errorf("invalid model name: only letters, digits, and . : / _ - are allowed")
			}

			var baseURL string
			if sp.name == llm.OpenAICompat {
				baseURL = ask("Base URL (e.g. http://localhost:8080/v1)", "")
				if inputErr != nil {
					return inputErr
				}
				if baseURL == "" {
					return fmt.Errorf("openai-compat needs a base URL")
				}
			}

			profileName := generateProfileName(cfg, sp.name, model)
			profile := config.Profile{Provider: sp.name, Model: model, BaseURL: baseURL}
			if sp.name == llm.Ollama {
				// Measured on qwen3.5: same accuracy, roughly half the latency.
				// Models without thinking ignore it.
				profile.ReasoningEffort = "none"
			}
			cfg.AddProfile(profileName, profile)
			if len(cfg.Profiles) == 1 || cfg.DefaultProfile == "" || !cfg.ProfileExists(cfg.DefaultProfile) {
				cfg.DefaultProfile = profileName
			}

			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("\nProfile created: %s (%s/%s)\n", profileName, sp.name, model)
			if env := llm.APIKeyEnv(sp.name); env != "" && llm.APIKey(sp.name) == "" {
				fmt.Printf("Set your API key: export %s=...\n", env)
			}
			if sp.hint != "" {
				fmt.Println(sp.hint)
			}
			if cfg.DefaultProfile == profileName {
				fmt.Println("Set as default profile.")
			} else {
				fmt.Printf("To make this the default, run: heyman profile set-default %s\n", profileName)
			}
			fmt.Printf("\nTry it out:\n  heyman ls how do I list files by size\n")
			return nil
		},
	}
}

type setupProvider struct {
	name, blurb, defaultModel, hint string
}

var setupProviders = []setupProvider{
	{llm.Anthropic, "Claude API (recommended)", "claude-haiku-4-5", ""},
	{llm.ClaudeCode, "use your Claude Code login via the claude CLI (slower)", "haiku", "Requires the `claude` CLI to be installed and logged in."},
	{llm.OpenAI, "OpenAI API", "", ""},
	{llm.OpenRouter, "OpenRouter (many hosted open-weight models)", "", ""},
	{llm.Google, "Gemini API", "", ""},
	{llm.Ollama, "local models via Ollama", "qwen3.5:4b", "Make sure Ollama is running (ollama serve) and the model is pulled (ollama pull <model>).\nThinking is turned off (reasoning_effort = \"none\"): it's much faster and just as accurate for heyman.\nOllama may give the model a very large context window; see the README for capping it to save memory."},
	{llm.OpenAICompat, "any OpenAI-compatible server (llama.cpp, vLLM, LM Studio…)", "", ""},
}

func profileDeleteCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "delete <profile-name>",
		Short: "Delete a profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profileName := args[0]

			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// Check if profile exists
			if !cfg.ProfileExists(profileName) {
				return profileNotFound(cfg, profileName)
			}

			// Confirm deletion unless --force
			if !force {
				fmt.Printf("Delete profile '%s'? [y/N] ", profileName)
				reader := bufio.NewReader(os.Stdin)
				response, _ := reader.ReadString('\n')
				response = strings.TrimSpace(strings.ToLower(response))
				if response != "y" && response != "yes" {
					fmt.Println("Cancelled.")
					return nil
				}
			}

			// Delete the profile
			newDefault, err := cfg.DeleteProfile(profileName)
			if err != nil {
				return fmt.Errorf("failed to delete profile: %w", err)
			}

			// Save config
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("Profile '%s' deleted.\n", profileName)

			if newDefault != "" {
				fmt.Printf("Default changed to '%s'. Use 'heyman profile set-default <name>' to change.\n", newDefault)
			}

			return nil
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "skip confirmation prompt")

	return cmd
}

func profileSetDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-default <profile-name>",
		Short: "Set the default profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profileName := args[0]

			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// Check if profile exists
			if !cfg.ProfileExists(profileName) {
				return profileNotFound(cfg, profileName)
			}

			// Update default profile
			if err := cfg.SetDefault(profileName); err != nil {
				return fmt.Errorf("failed to set default: %w", err)
			}

			// Save config
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("Default profile set to: %s\n", profileName)

			// Show profile details
			profile := cfg.Profiles[profileName]
			fmt.Printf("  Provider: %s\n", profile.Provider)
			fmt.Printf("  Model:    %s\n", profile.Model)

			return nil
		},
	}
}

func profileShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [profile-name]",
		Short: "Show profile details",
		Long:  "Show details of a profile. If no name given, shows the default profile.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			var profileName string
			if len(args) > 0 {
				profileName = args[0]
			} else {
				profileName = cfg.DefaultProfile
				if profileName == "" {
					return fmt.Errorf("no default profile set. Run 'heyman profile setup' to create one")
				}
			}

			profile, ok := cfg.Profiles[profileName]
			if !ok {
				return profileNotFound(cfg, profileName)
			}

			isDefault := ""
			if profileName == cfg.DefaultProfile {
				isDefault = " (default)"
			}

			fmt.Printf("Profile: %s%s\n", profileName, isDefault)
			fmt.Printf("  Provider:       %s\n", profile.Provider)
			fmt.Printf("  Model:          %s\n", profile.Model)
			if profile.BaseURL != "" {
				fmt.Printf("  Base URL:       %s\n", profile.BaseURL)
			}
			if profile.ReasoningEffort != "" {
				fmt.Printf("  Reasoning:      %s\n", profile.ReasoningEffort)
			}
			fmt.Printf("  Use with:       heyman --profile %s …  (or --model %s)\n", profileName, profile.Spec())

			return nil
		},
	}
}

func testConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test-config",
		Short: "Validate and test all profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			if len(cfg.Profiles) == 0 {
				return fmt.Errorf("no profiles configured")
			}

			fmt.Println("Testing profiles...")
			fmt.Println()

			hasErrors := false

			for _, name := range cfg.SortedProfileNames() {
				profile := cfg.Profiles[name]
				fmt.Printf("Testing %s (%s %s)... ", name, profile.Provider, profile.Model)

				if err := checkProfile(cmd.Context(), profile); err != nil {
					fmt.Println(err)
					hasErrors = true
					continue
				}

				fmt.Println("OK")
			}

			if hasErrors {
				return fmt.Errorf("some profiles have configuration issues")
			}

			fmt.Println("\nAll profiles configured correctly")
			return nil
		},
	}
}

func cacheStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cache-stats",
		Short: "Show cache statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			cacheManager := cache.New(cfg.CacheDays)
			stats, err := cacheManager.GetStats()
			if err != nil {
				return fmt.Errorf("failed to get cache stats: %w", err)
			}

			fmt.Printf("Cache Statistics:\n")
			fmt.Printf("  Total entries:    %d\n", stats.TotalEntries)
			fmt.Printf("  Total size:       %.2f KB\n", float64(stats.TotalSizeBytes)/1024.0)
			fmt.Printf("  Total hits:       %d\n", stats.TotalHits)

			if stats.OldestEntry != nil {
				fmt.Printf("  Oldest entry:     %s\n", stats.OldestEntry.Format("2006-01-02 15:04:05"))
			}
			if stats.NewestEntry != nil {
				fmt.Printf("  Newest entry:     %s\n", stats.NewestEntry.Format("2006-01-02 15:04:05"))
			}

			fmt.Printf("  Cache directory:  %s\n", config.GetCacheDir())
			fmt.Printf("  Max age:          %d days\n", cfg.CacheDays)

			return nil
		},
	}
}

func clearCacheCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear-cache",
		Short: "Clear all cached responses",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			cacheManager := cache.New(cfg.CacheDays)
			removed, err := cacheManager.Clear()
			if err != nil {
				return fmt.Errorf("failed to clear cache: %w", err)
			}

			fmt.Printf("Cleared %d cached entries\n", removed)
			return nil
		},
	}
}

func profileNotFound(cfg *config.Config, name string) error {
	names := cfg.SortedProfileNames()
	if len(names) == 0 {
		return fmt.Errorf("profile %q not found: no profiles configured (run: heyman profile setup)", name)
	}
	return fmt.Errorf("profile %q not found (available: %s)", name, strings.Join(names, ", "))
}

// checkProfile validates a profile without spending tokens: the provider and
// model are set, credentials are present, and the claude CLI or local Ollama
// server is reachable.
func checkProfile(ctx context.Context, p config.Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	spec, err := llm.ParseSpec(p.Spec())
	if err != nil {
		return err
	}
	if spec.Provider == llm.ClaudeCode {
		if _, err := exec.LookPath("claude"); err != nil {
			return fmt.Errorf("the claude CLI is not on PATH")
		}
		return nil
	}
	if _, err := llm.NewLanguageModel(ctx, spec, llm.Options{BaseURL: p.BaseURL}); err != nil {
		return err
	}
	if spec.Provider == llm.Ollama {
		base := p.BaseURL
		if base == "" {
			base = llm.OllamaBaseURL()
		}
		if err := checkOllamaModel(ctx, base, spec.Model); err != nil {
			return err
		}
	}
	return nil
}

// checkOllamaModel asks the server's OpenAI-compatible /models endpoint
// whether model is available.
func checkOllamaModel(ctx context.Context, base, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach Ollama at %s (is `ollama serve` running?)", base)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama at %s: %s", base, resp.Status)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return fmt.Errorf("ollama at %s: %w", base, err)
	}
	for _, m := range list.Data {
		if m.ID == model || (!strings.Contains(model, ":") && m.ID == model+":latest") {
			return nil
		}
	}
	return fmt.Errorf("model %s is not pulled (run: ollama pull %s)", model, model)
}
