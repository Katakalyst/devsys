package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:     "devsys",
	Short:   "Developer system container manager",
	Version: currentVersion,
	Long: `devsys manages containerised development environments using Podman.
Each project gets an isolated container built on devsys-base with
Claude, Codex, and GitLab integration baked in.`,
	// Loads the stored GitHub base URL from the bootstrap secret so that
	// PlatformFromURL correctly classifies GHE remote URLs everywhere (init,
	// rebuild, enter, auth) without requiring --github-url on every invocation.
	// Runs before every command — silently skipped if no bootstrap secret exists.
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		loadGitHubHostFromBootstrap()
	},
	// Shows a throttled, non-blocking "devsys is outdated" notice after any
	// command (devsys CLI Spec, Section 12.5 — modeled on npm's own
	// update-notifier: a background check on ordinary use, not a separate
	// command you have to remember to run). Skipped for `update` itself: a
	// successful self-replace doesn't change currentVersion in this already-
	// running process (it's a compile-time value), so checking again here
	// would print a confusing "outdated" notice about the binary that was
	// just replaced.
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		if cmd.Name() == "update" {
			return
		}
		if !checkThrottled("cli-version") {
			warnIfCLIOutdated()
			markChecked("cli-version")
		}
	},
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(authCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(rebuildCmd)
	rootCmd.AddCommand(enterCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(rmCmd)
	rootCmd.AddCommand(cleanCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(doctorCmd)
	rootCmd.AddCommand(uninstallCmd)
}
