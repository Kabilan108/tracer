// Package cmd contains CLI command implementations for the Tracer CLI.
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// DisplayHelp prints the command's help text.
// Exported because it's used by both the help command and the root command's Run handler.
func DisplayHelp(cmd *cobra.Command) {
	_ = cmd.Help()
}

// CreateHelpCommand creates a custom help command that displays the Tracer logo.
// rootCmd is needed to look up subcommands when the user types "tracer help <command>".
func CreateHelpCommand(rootCmd *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:     "help [command]",
		Aliases: []string{"h"},
		Short:   "Help about any command",
		Run: func(cmd *cobra.Command, args []string) {
			// If a subcommand is specified, determine if it's valid
			if len(args) > 0 {
				targetCmd, _, err := rootCmd.Find(args)
				if err != nil {
					fmt.Printf("Unknown command: %s\n", args[0])
					DisplayHelp(rootCmd)
				} else {
					DisplayHelp(targetCmd)
				}
			} else {
				// No command specified - general help requested
				DisplayHelp(rootCmd)
			}
		},
	}
}
