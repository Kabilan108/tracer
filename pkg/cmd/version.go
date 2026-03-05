// Package cmd contains CLI command implementations for the Tracer CLI.
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tracer-ai/tracer-cli/pkg/ui"
)

// CreateVersionCommand creates the version command.
// The version string is passed in because it's set at build time in main.go.
func CreateVersionCommand(version string) *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Aliases: []string{"v", "ver"},
		Short:   "Show Tracer version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("%s %s\n", ui.Command("tracer"), version)
		},
	}
}
