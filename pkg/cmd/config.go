// Package cmd contains CLI command implementations for the Tracer CLI.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tracer-ai/tracer-cli/pkg/config"
	"github.com/tracer-ai/tracer-cli/pkg/ui"
)

// CreateConfigCommand creates the config command tree.
func CreateConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config [command]",
		Short: "Manage Tracer configuration",
		Long: "Manage the user configuration file.\n\n" +
			"Config path: ~/.config/tracer/config.toml (or $XDG_CONFIG_HOME/tracer/config.toml).",
	}

	cmd.AddCommand(CreateConfigInitCommand())
	cmd.AddCommand(CreateConfigCheckCommand())
	return cmd
}

// CreateConfigInitCommand creates the config init command.
func CreateConfigInitCommand() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a default user config file",
		Long: "Create a commented default config file at the user config path.\n\n" +
			"Path: ~/.config/tracer/config.toml (or $XDG_CONFIG_HOME/tracer/config.toml).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			configPath := config.GetUserConfigPath()
			if configPath == "" {
				return fmt.Errorf("could not determine config path")
			}
			if err := writeDefaultConfig(configPath, force); err != nil {
				return err
			}

			fmt.Printf("%s Created config file\n", ui.Success("OK"))
			fmt.Printf("Path: %s\n", configPath)
			fmt.Printf("Run %s to validate it.\n", tracerCommand("config", "check"))
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config file")
	return cmd
}

func writeDefaultConfig(configPath string, force bool) error {
	path := strings.TrimSpace(configPath)
	if path == "" {
		return fmt.Errorf("config path is empty")
	}

	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return fmt.Errorf("config path is a directory: %s", path)
		}
		if !force {
			return fmt.Errorf("config file already exists: %s (use --force to overwrite)", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to inspect config path: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	if err := os.WriteFile(path, []byte(config.DefaultTemplate()), 0o644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}
