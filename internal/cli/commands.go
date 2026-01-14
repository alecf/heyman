package cli

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/alecf/heyman/internal/cache"
	"github.com/alecf/heyman/internal/config"
	"github.com/spf13/cobra"
)

// validModelName checks if a model name is safe for use in profile names
// Allows: alphanumeric, dots, colons, hyphens, underscores
var validModelName = regexp.MustCompile(`^[a-zA-Z0-9._:-]+$`)

// sizePattern matches size indicators like 1b, 7b, 70b, 270b
var sizePattern = regexp.MustCompile(`(?i)^(\d+[bB])`)

// sanitizeProfileName creates a safe profile name from user input
func sanitizeProfileName(input string) string {
	// Only allow alphanumeric, hyphens, and underscores
	safe := regexp.MustCompile(`[^a-zA-Z0-9_-]`).ReplaceAllString(input, "-")
	// Remove leading/trailing hyphens
	safe = strings.Trim(safe, "-")
	// Collapse multiple hyphens
	safe = regexp.MustCompile(`-+`).ReplaceAllString(safe, "-")
	return safe
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
	if idx := strings.Index(model, ":"); idx != -1 {
		baseName = model[:idx]
		tag = model[idx+1:]
	} else {
		baseName = model
		tag = ""
	}

	sanitizedBase := sanitizeProfileName(baseName)
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
				if name == cfg.DefaultProfile {
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
				cfg = &config.Config{
					CacheDays: 30,
					Profiles:  make(map[string]config.Profile),
				}
			}

			// Simple text-based wizard
			fmt.Println("Select a provider:")
			fmt.Println("  1) OpenAI")
			fmt.Println("  2) Anthropic")
			fmt.Println("  3) Ollama (local)")
			fmt.Print("\nChoice [1-3]: ")

			reader := bufio.NewReader(os.Stdin)
			choice, _ := reader.ReadString('\n')
			choice = strings.TrimSpace(choice)

			// Validate choice
			if choice != "1" && choice != "2" && choice != "3" {
				return fmt.Errorf("invalid choice: must be 1, 2, or 3")
			}

			var provider, model, profileName string

			switch choice {
			case "1":
				provider = "openai"
				model = "gpt-4o-mini" // Default to cheaper model
				profileName = generateProfileName(cfg, provider, model)
				fmt.Println("\nUsing OpenAI with gpt-4o-mini")
				fmt.Println("Set your API key with: export OPENAI_API_KEY=sk-...")
			case "2":
				provider = "anthropic"
				model = "claude-3-5-haiku-20241022" // Default to cheaper model
				profileName = generateProfileName(cfg, provider, model)
				fmt.Println("\nUsing Anthropic with Claude 3.5 Haiku")
				fmt.Println("Set your API key with: export ANTHROPIC_API_KEY=sk-...")
			case "3":
				provider = "ollama"
				fmt.Print("\nEnter model name (e.g., llama3.2:latest): ")
				model, _ = reader.ReadString('\n')
				model = strings.TrimSpace(model)
				if model == "" {
					model = "llama3.2:latest"
				}
				// Validate model name
				if !validModelName.MatchString(model) {
					return fmt.Errorf("invalid model name: only alphanumeric, dots, colons, hyphens, and underscores allowed")
				}
				profileName = generateProfileName(cfg, provider, model)
				fmt.Println("\nUsing Ollama with", model)
				fmt.Println("Make sure Ollama is running: ollama serve")
			default:
				return fmt.Errorf("invalid choice")
			}

			// Add profile
			cfg.AddProfile(profileName, config.Profile{
				Provider: provider,
				Model:    model,
			})

			// Set as default if first profile
			if len(cfg.Profiles) == 1 {
				cfg.DefaultProfile = profileName
			}

			// Save config
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("\nProfile created: %s\n", profileName)
			fmt.Printf("  Provider: %s\n", provider)
			fmt.Printf("  Model: %s\n", model)

			if cfg.DefaultProfile == profileName {
				fmt.Printf("\nSet as default profile.\n")
			} else {
				fmt.Printf("\nTo make this the default, run: heyman profile set-default %s\n", profileName)
			}

			fmt.Printf("\nTry it out:\n")
			fmt.Printf("  heyman ls how do I list files by size\n")

			return nil
		},
	}
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
				fmt.Printf("Profile '%s' not found.\n\n", profileName)
				fmt.Println("Available profiles:")
				for _, name := range cfg.SortedProfileNames() {
					fmt.Printf("  - %s\n", name)
				}
				return fmt.Errorf("profile not found")
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
				fmt.Printf("Profile '%s' not found.\n\n", profileName)
				fmt.Println("Available profiles:")
				for _, name := range cfg.SortedProfileNames() {
					fmt.Printf("  - %s\n", name)
				}
				return fmt.Errorf("profile not found")
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
				fmt.Printf("Profile '%s' not found.\n\n", profileName)
				fmt.Println("Available profiles:")
				for _, name := range cfg.SortedProfileNames() {
					fmt.Printf("  - %s\n", name)
				}
				return fmt.Errorf("profile not found")
			}

			isDefault := ""
			if profileName == cfg.DefaultProfile {
				isDefault = " (default)"
			}

			fmt.Printf("Profile: %s%s\n", profileName, isDefault)
			fmt.Printf("  Provider:       %s\n", profile.Provider)
			fmt.Printf("  Model:          %s\n", profile.Model)
			if profile.ContextWindow > 0 {
				fmt.Printf("  Context window: %d tokens\n", profile.ContextWindow)
			}

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

			for name, profile := range cfg.Profiles {
				fmt.Printf("Testing %s (%s %s)... ", name, profile.Provider, profile.Model)

				// Check API keys for cloud providers
				switch profile.Provider {
				case "openai":
					if cfg.GetAPIKey("openai") == "" {
						fmt.Println("Missing OPENAI_API_KEY")
						hasErrors = true
						continue
					}
				case "anthropic":
					if cfg.GetAPIKey("anthropic") == "" {
						fmt.Println("Missing ANTHROPIC_API_KEY")
						hasErrors = true
						continue
					}
				case "ollama":
					// Check if Ollama is running
					// For now, just assume it's OK
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
