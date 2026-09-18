package cmd

import (
	"fmt"
	"strings"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all devsys projects and their status",
	RunE:    runList,
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show status of all devsys projects (alias for list)",
	RunE:  runList,
}

func runList(cmd *cobra.Command, args []string) error {
	containers, err := podman.ListDevsysContainers()
	if err != nil {
		return fmt.Errorf("cannot list containers: %w", err)
	}
	volumes, err := podman.ListDevsysVolumes()
	if err != nil {
		return fmt.Errorf("cannot list volumes: %w", err)
	}

	if len(containers) == 0 {
		fmt.Println("No devsys containers found.")
	} else {
		fmt.Printf("%-30s %-15s %-20s\n", "CONTAINER", "STATUS", "IMAGE")
		fmt.Println(strings.Repeat("-", 70))
		for _, c := range containers {
			name := containerField(c, "Names")
			status := containerField(c, "State")
			image := containerField(c, "Image")
			if image == "" {
				image = containerField(c, "ImageName")
			}
			fmt.Printf("%-30s %-15s %-20s\n", name, status, image)
		}
	}

	fmt.Println()
	if len(volumes) == 0 {
		fmt.Println("No devsys volumes found.")
	} else {
		fmt.Printf("%-40s\n", "VOLUME")
		fmt.Println(strings.Repeat("-", 40))
		for _, v := range volumes {
			name := containerField(v, "Name")
			if name == "" {
				name = containerField(v, "VolumeName")
			}
			fmt.Printf("%-40s\n", name)
		}
	}
	return nil
}

// containerField extracts a string field from a container/volume map, handling
// both direct string values and single-element slices (Podman JSON varies by version).
func containerField(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case []interface{}:
		if len(val) > 0 {
			if s, ok := val[0].(string); ok {
				return s
			}
		}
	}
	return fmt.Sprintf("%v", v)
}
