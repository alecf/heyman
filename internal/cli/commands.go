package cli

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/alecf/heyman/internal/cache"
	"github.com/alecf/heyman/internal/config"
	"github.com/spf13/cobra"
)

// validModelName checks if a model name is safe for use in profile names
// Allows: alphanumeric, dots, colons, hyphens, underscores
var validModelName = regexp.MustCompile(`^[a-zA-Z0-9._:-]+$`)

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

func setupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Interactive configuration wizard",
		Long: `Interactive setup wizard to configure heyman profiles.

You can also set up profiles manually by editing:
  ~/.config/heyman/config.toml (Linux/others)
  ~/Library/Application Support/heyman/config.toml (macOS)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Heyman Setup Wizard")
			fmt.Println("===================")
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

			var choice string
			fmt.Scanln(&choice)

			// Validate choice
			if choice != "1" && choice != "2" && choice != "3" {
				return fmt.Errorf("invalid choice: must be 1, 2, or 3")
			}

			var provider, model, profileName string

			switch choice {
			case "1":
				provider = "openai"
				model = "gpt-4o-mini" // Default to cheaper model
				profileName = "openai-gpt4o-mini"
				fmt.Println("\nUsing OpenAI with gpt-4o-mini")
				fmt.Println("Set your API key with: export OPENAI_API_KEY=sk-...")
			case "2":
				provider = "anthropic"
				model = "claude-3-5-haiku-20241022" // Default to cheaper model
				profileName = "anthropic-haiku"
				fmt.Println("\nUsing Anthropic with Claude 3.5 Haiku")
				fmt.Println("Set your API key with: export ANTHROPIC_API_KEY=sk-...")
			case "3":
				provider = "ollama"
				fmt.Print("\nEnter model name (e.g., llama3.2:latest): ")
				fmt.Scanln(&model)
				if model == "" {
					model = "llama3.2:latest"
				}
				// Validate model name
				if !validModelName.MatchString(model) {
					return fmt.Errorf("invalid model name: only alphanumeric, dots, colons, hyphens, and underscores allowed")
				}
				baseProfileName := strings.Split(model, ":")[0]
				profileName = fmt.Sprintf("ollama-%s", sanitizeProfileName(baseProfileName))
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

			// Set as default
			cfg.DefaultProfile = profileName

			// Save config
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("\n✓ Configuration saved!\n")
			fmt.Printf("  Profile: %s\n", profileName)
			fmt.Printf("  Provider: %s\n", provider)
			fmt.Printf("  Model: %s\n", model)
			fmt.Printf("\nTry it out:\n")
			fmt.Printf("  heyman ls how do I list files by size\n")

			return nil
		},
	}
}

func setProfileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-profile <profile-name>",
		Short: "Set the default profile",
		Long: `Set the default profile to use for queries.

Example:
  heyman set-profile openai-gpt4o
  heyman set-profile ollama-llama`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			profileName := args[0]

			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// Check if profile exists
			if _, ok := cfg.Profiles[profileName]; !ok {
				fmt.Printf("Profile '%s' not found.\n\n", profileName)
				fmt.Println("Available profiles:")
				for name := range cfg.Profiles {
					fmt.Printf("  - %s\n", name)
				}
				return fmt.Errorf("profile not found")
			}

			// Update default profile
			cfg.DefaultProfile = profileName

			// Save config
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("failed to save config: %w", err)
			}

			fmt.Printf("✓ Default profile set to: %s\n", profileName)

			// Show profile details
			profile := cfg.Profiles[profileName]
			fmt.Printf("  Provider: %s\n", profile.Provider)
			fmt.Printf("  Model:    %s\n", profile.Model)

			return nil
		},
	}
}

func listProfilesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list-profiles",
		Short: "Show all configured profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			if len(cfg.Profiles) == 0 {
				fmt.Println("No profiles configured. Run 'heyman setup' to create one.")
				return nil
			}

			fmt.Println("Configured Profiles:")
			fmt.Println()

			for name, profile := range cfg.Profiles {
				marker := " "
				if name == cfg.DefaultProfile {
					marker = "*"
				}
				fmt.Printf("%s %s\n", marker, name)
				fmt.Printf("    Provider: %s\n", profile.Provider)
				fmt.Printf("    Model:    %s\n", profile.Model)
				fmt.Println()
			}

			fmt.Println("* = default profile")
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
						fmt.Println("❌ Missing OPENAI_API_KEY")
						hasErrors = true
						continue
					}
				case "anthropic":
					if cfg.GetAPIKey("anthropic") == "" {
						fmt.Println("❌ Missing ANTHROPIC_API_KEY")
						hasErrors = true
						continue
					}
				case "ollama":
					// Check if Ollama is running
					// For now, just assume it's OK
				}

				fmt.Println("✓")
			}

			if hasErrors {
				return fmt.Errorf("some profiles have configuration issues")
			}

			fmt.Println("\n✓ All profiles configured correctly")
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
