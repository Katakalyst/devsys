package podman

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// DryRun, when true, causes all mutating podman operations to print what
// they would do instead of executing. Read-only operations (inspect, ps,
// version, etc.) always run regardless of this flag.
//
// In production builds this is always false. The dev build tag sets it to
// true via an init() in dryrun_dev.go, which is excluded from prod binaries.
var DryRun bool

// ExecCmd is the function used to construct exec.Cmd instances for podman
// invocations. Override in tests (via internal/podmanfake) to inject a fake
// podman binary without touching the file system or real containers.
var ExecCmd = exec.Command

// DryRunOutput is where dry-run descriptions are written. Defaults to
// os.Stdout; override in tests to capture output.
var DryRunOutput io.Writer = os.Stdout

// isReadOnly reports whether a podman invocation is safe to run even during
// dry-run (i.e. it only reads state, never changes it).
func isReadOnly(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "--version", "version", "info", "inspect", "ps":
		return true
	case "secret", "volume", "image", "container":
		if len(args) >= 2 {
			switch args[1] {
			case "inspect", "ls", "list":
				return true
			}
		}
	}
	return false
}

// RunPodman executes a podman command and returns combined stdout output.
// stderr is suppressed unless the command fails, in which case it's included
// in the error. Mutating commands are skipped (returning "") when DryRun is true.
func RunPodman(args ...string) (string, error) {
	if DryRun && !isReadOnly(args) {
		fmt.Fprintf(DryRunOutput, "[dry-run] podman %s\n", strings.Join(args, " "))
		return "", nil
	}
	cmd := ExecCmd("podman", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("podman %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// RunPodmanLive runs a podman command with stdout and stderr connected
// directly to the terminal (for commands like pull/build that stream output).
// Mutating commands are skipped when DryRun is true.
func RunPodmanLive(args ...string) error {
	if DryRun && !isReadOnly(args) {
		fmt.Fprintf(DryRunOutput, "[dry-run] podman %s\n", strings.Join(args, " "))
		return nil
	}
	cmd := ExecCmd("podman", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ExecInteractive runs a command inside a container with stdin/stdout/stderr
// connected directly to the calling terminal.
// Skipped (prints intent) when DryRun is true.
func ExecInteractive(containerName string, command ...string) error {
	args := append([]string{"exec", "-it", containerName}, command...)
	if DryRun {
		fmt.Fprintf(DryRunOutput, "[dry-run] podman %s\n", strings.Join(args, " "))
		return nil
	}
	cmd := ExecCmd("podman", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// SecretExists reports whether a Podman secret with the given name exists.
func SecretExists(name string) bool {
	_, err := RunPodman("secret", "inspect", name)
	return err == nil
}

// GetSecretValue reads the plaintext value of a Podman secret.
// Requires Podman 4.7+ (--showsecret flag).
func GetSecretValue(name string) (string, error) {
	out, err := RunPodman("secret", "inspect", "--showsecret", "--format", "{{.SecretData}}", name)
	if err != nil {
		return "", fmt.Errorf("cannot read secret %s: %w", name, err)
	}
	return strings.TrimSpace(out), nil
}

// secretInspectOutput is a partial representation of `podman secret inspect` JSON.
type secretInspectOutput struct {
	Spec struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Spec"`
}

// GetSecretLabels returns the labels attached to a Podman secret.
func GetSecretLabels(name string) (map[string]string, error) {
	// `podman secret inspect` outputs JSON by default; --format json is NOT
	// needed and is treated as a literal Go template string in some Podman
	// versions, producing the text "json" instead of JSON output.
	out, err := RunPodman("secret", "inspect", name)
	if err != nil {
		return nil, fmt.Errorf("cannot inspect secret %s: %w", name, err)
	}
	var results []secretInspectOutput
	if err := json.Unmarshal([]byte(out), &results); err != nil {
		return nil, fmt.Errorf("cannot parse secret inspect output: %w", err)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("secret %s not found", name)
	}
	labels := results[0].Spec.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return labels, nil
}

// ContainerExists reports whether a container with the given name exists (any state).
func ContainerExists(name string) bool {
	_, err := RunPodman("container", "inspect", name)
	return err == nil
}

// ContainerIsRunning reports whether a container is in the running state.
func ContainerIsRunning(name string) bool {
	out, err := RunPodman("inspect", "--format", "{{.State.Running}}", name)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

// VolumeExists reports whether a named volume exists.
func VolumeExists(name string) bool {
	_, err := RunPodman("volume", "inspect", name)
	return err == nil
}

// CreateSecretFromStdin creates a Podman secret by piping value via stdin.
// labels is a map of key=value labels to attach.
// Skipped (prints intent) when DryRun is true.
func CreateSecretFromStdin(name, value string, labels map[string]string) error {
	args := []string{"secret", "create"}
	for k, v := range labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, name, "-")

	if DryRun {
		fmt.Fprintf(DryRunOutput, "[dry-run] podman %s (value redacted)\n", strings.Join(args, " "))
		return nil
	}

	cmd := ExecCmd("podman", args...)
	cmd.Stdin = strings.NewReader(value)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("podman secret create %s: %w\n%s", name, err, stderr.String())
	}
	return nil
}

// DeleteSecret removes a Podman secret by name.
func DeleteSecret(name string) error {
	_, err := RunPodman("secret", "rm", name)
	return err
}

// ListDevsysContainers returns names of all containers labelled devsys=true.
func ListDevsysContainers() ([]map[string]interface{}, error) {
	out, err := RunPodman("ps", "-a", "--filter", "label=devsys=true", "--format", "json")
	if err != nil {
		return nil, err
	}
	if out == "" || out == "null" {
		return nil, nil
	}
	var containers []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &containers); err != nil {
		return nil, fmt.Errorf("cannot parse container list: %w", err)
	}
	return containers, nil
}

// ListDevsysVolumes returns all volumes labelled devsys=true.
func ListDevsysVolumes() ([]map[string]interface{}, error) {
	out, err := RunPodman("volume", "ls", "--filter", "label=devsys=true", "--format", "json")
	if err != nil {
		return nil, err
	}
	if out == "" || out == "null" {
		return nil, nil
	}
	var volumes []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &volumes); err != nil {
		return nil, fmt.Errorf("cannot parse volume list: %w", err)
	}
	return volumes, nil
}
