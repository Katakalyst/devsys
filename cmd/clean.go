package cmd

import (
	"bufio"
	"fmt"
	"os"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var cleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Prune stopped devsys containers and dangling images",
	RunE:  runClean,
}

func runClean(cmd *cobra.Command, args []string) error {
	reader := bufio.NewReader(os.Stdin)

	if !confirm(reader, "Remove all stopped devsys containers and dangling images?") {
		fmt.Println("Aborted.")
		return nil
	}

	// Remove stopped devsys containers.
	containers, err := podman.ListDevsysContainers()
	if err != nil {
		return fmt.Errorf("cannot list containers: %w", err)
	}
	removed := 0
	for _, c := range containers {
		name := containerField(c, "Names")
		state := containerField(c, "State")
		if state == "exited" || state == "stopped" || state == "created" {
			fmt.Printf("Removing stopped container %s ...\n", name)
			if _, err := podman.RunPodman("rm", name); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: %v\n", err)
			} else {
				removed++
			}
		}
	}
	fmt.Printf("Removed %d stopped container(s).\n", removed)

	// Prune dangling images.
	fmt.Println("Pruning dangling images...")
	out, err := podman.RunPodman("image", "prune", "-f")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot prune images: %v\n", err)
	} else {
		if out != "" {
			fmt.Println(out)
		} else {
			fmt.Println("  No dangling images to remove.")
		}
	}
	return nil
}
