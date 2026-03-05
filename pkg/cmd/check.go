// Package cmd contains CLI command implementations for the Tracer CLI.
package cmd

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tracer-ai/tracer-cli/pkg/config"
	"github.com/tracer-ai/tracer-cli/pkg/spi/factory"
	"github.com/tracer-ai/tracer-cli/pkg/ui"
	"github.com/tracer-ai/tracer-cli/pkg/utils"
)

// CreateCheckCommand dynamically creates the check command with provider information.
func CreateCheckCommand() *cobra.Command {
	registry := factory.GetRegistry()
	ids := registry.ListIDs()

	var examplesBuilder strings.Builder
	examplesBuilder.WriteString(`
# Check all coding agents
tracer check`)

	if len(ids) > 0 {
		examplesBuilder.WriteString("\n\n# Check specific coding agent")
		for _, id := range ids {
			fmt.Fprintf(&examplesBuilder, "\ntracer check %s", id)
		}
		fmt.Fprintf(&examplesBuilder, "\n\n# Check a specific coding agent with a custom command\ntracer check %s -c \"/custom/path/to/agent\"", ids[0])
	}
	examples := examplesBuilder.String()

	cmd := &cobra.Command{
		Use:   "check [provider-id]",
		Short: "Check if the configuration is valid and terminal coding agents are properly installed",
		Long: `Check if the configuration is valid and terminal coding agents are properly installed and can be invoked by Tracer.

Ensures the user-level configuration file is valid.

By default, checks all registered coding agents providers.
Specify a specific agent ID to check only a specific coding agent.`,
		Example: examples,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slog.Info("Running in check-install mode")
			registry := factory.GetRegistry()

			customCmd, _ := cmd.Flags().GetString("command")
			if customCmd != "" && len(args) == 0 {
				ids := registry.ListIDs()
				example := "tracer check <provider> -c \"/custom/path/to/agent\""
				if len(ids) > 0 {
					example = fmt.Sprintf("tracer check %s -c \"/custom/path/to/agent\"", ids[0])
				}
				return utils.ValidationError{
					Message: "The -c/--command flag requires a provider to be specified.\n" +
						"Example: " + example,
				}
			}

			configOK := checkConfigFiles()
			printDivider()

			var providerErr error
			if len(args) == 0 {
				providerErr = checkAllProviders(registry)
			} else {
				providerErr = checkSingleProvider(registry, args[0], customCmd)
			}

			if !configOK && providerErr != nil {
				return providerErr
			}
			if !configOK {
				return errors.New("config check failed")
			}
			return providerErr
		},
	}

	cmd.Flags().StringP("command", "c", "", "custom agent execution command for the provider")
	return cmd
}

func tracerCommand(args ...string) string {
	return strings.TrimSpace(ui.Command("tracer") + " " + strings.Join(args, " "))
}

func printDivider() {
	fmt.Println(strings.Repeat("-", 72))
}

func checkSingleProvider(registry *factory.Registry, providerID, customCmd string) error {
	provider, err := registry.Get(providerID)
	if err != nil {
		fmt.Printf("%s Provider %q is not registered\n\n", ui.Error("Error"), providerID)

		ids := registry.ListIDs()
		if len(ids) > 0 {
			fmt.Println(ui.Section("Registered providers"))
			for _, id := range ids {
				if p, _ := registry.Get(id); p != nil {
					fmt.Printf("%s  %s\n", ui.Command(id), p.Name())
				}
			}
			fmt.Printf("\nExample: %s\n", tracerCommand("check", ids[0]))
		}
		return err
	}

	result := provider.Check(customCmd)
	if result.Success {
		fmt.Printf("%s %s is installed and ready\n", ui.Success("OK"), provider.Name())
		fmt.Printf("Version:  %s\n", result.Version)
		fmt.Printf("Location: %s\n", result.Location)
		fmt.Printf("Status:   %s\n", ui.Success("ready"))
		fmt.Println()
		fmt.Println(ui.Section("Next"))
		normalizedID := strings.ToLower(providerID)
		fmt.Println(tracerCommand("sync", normalizedID))
		fmt.Println(tracerCommand("watch", normalizedID))
		return nil
	}

	fmt.Printf("%s %s check failed\n", ui.Error("Failed"), provider.Name())
	if result.ErrorMessage != "" {
		fmt.Printf("\n%s\n", result.ErrorMessage)
	}
	return errors.New("check failed")
}

func checkAllProviders(registry *factory.Registry) error {
	ids := registry.ListIDs()
	anySuccess := false

	type providerInfo struct {
		id string
	}
	var successfulProviders []providerInfo

	for i, id := range ids {
		if i > 0 {
			printDivider()
		}

		provider, _ := registry.Get(id)
		result := provider.Check("")

		if result.Success {
			anySuccess = true
			successfulProviders = append(successfulProviders, providerInfo{id: id})
			fmt.Printf("%s %s is installed and ready\n", ui.Success("OK"), provider.Name())
			fmt.Printf("Version:  %s\n", result.Version)
			fmt.Printf("Location: %s\n", result.Location)
			fmt.Printf("Status:   %s\n", ui.Success("ready"))
			continue
		}

		fmt.Printf("%s %s check failed\n", ui.Error("Failed"), provider.Name())
		if result.ErrorMessage != "" {
			lines := strings.Split(result.ErrorMessage, "\n")
			fmt.Printf("Error: %s\n", strings.TrimSpace(lines[0]))
		}
	}

	printDivider()
	if anySuccess {
		fmt.Println(ui.Section("Next"))
		for _, info := range successfulProviders {
			fmt.Println(tracerCommand("sync", info.id))
			fmt.Println(tracerCommand("watch", info.id))
		}
		return nil
	}

	fmt.Printf("%s No providers are currently available\n", ui.Warning("Warning"))
	fmt.Println("Install at least one provider to use Tracer.")

	if len(ids) > 0 {
		fmt.Printf("Example: %s\n", tracerCommand("check", ids[0]))
	} else {
		fmt.Printf("Example: %s\n", tracerCommand("check", "<provider>"))
	}

	return errors.New("check failed")
}

// checkConfigFiles validates the user-level config file.
// Returns true if all existing config files are valid, false if any have errors.
func checkConfigFiles() bool {
	fmt.Println(ui.Section("Configuration"))

	allOK := true
	checked := false

	userPath := config.GetUserConfigPath()
	if userPath != "" {
		result := config.ValidateConfigFile(userPath)
		checked = true
		printConfigResult("User config", result)
		if result.Exists && (!result.ValidTOML || len(result.UnknownKeys) > 0) {
			allOK = false
		}
	}

	if !checked {
		fmt.Printf("%s Could not determine config file paths\n", ui.Warning("Warning"))
	}

	return allOK
}

func printConfigResult(label string, result config.ConfigValidationResult) {
	if !result.Exists {
		fmt.Printf("%s %s: not found\n", ui.Warning("Warning"), label)
		fmt.Printf("Path: %s\n\n", result.Path)
		return
	}

	if !result.ValidTOML {
		fmt.Printf("%s %s: invalid TOML\n", ui.Error("Error"), label)
		fmt.Printf("Path:  %s\n", result.Path)
		fmt.Printf("Error: %s\n\n", result.ParseError)
		return
	}

	if len(result.UnknownKeys) > 0 {
		fmt.Printf("%s %s: valid TOML with unknown keys\n", ui.Warning("Warning"), label)
		fmt.Printf("Path: %s\n", result.Path)
		for _, key := range result.UnknownKeys {
			fmt.Printf("Unknown key: %s\n", key)
		}
		fmt.Println()
		return
	}

	fmt.Printf("%s %s: valid\n", ui.Success("OK"), label)
	fmt.Printf("Path: %s\n\n", result.Path)
}
