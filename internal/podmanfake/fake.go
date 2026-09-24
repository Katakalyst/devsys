// Package podmanfake provides test infrastructure for injecting a fake podman
// binary into tests via the TestHelperProcess pattern.
//
// Usage in each test package:
//
//  1. Add a TestFakePodman function that calls podmanfake.Handle():
//
//	   func TestFakePodman(t *testing.T) { podmanfake.Handle() }
//
//  2. In your actual test, call Install to swap out podman.ExecCmd for the
//     duration of the test and get back a Recorder:
//
//	   rec := podmanfake.Install(t, podmanfake.Options{ContainerExists: true})
//	   // ... call the function under test ...
//	   if !rec.HasCall("start", "devsys-myproject") {
//	       t.Error("expected podman start to be called")
//	   }
package podmanfake

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/katakalyst/devsys/internal/podman"
)

// Options configures the canned responses the fake podman will return.
type Options struct {
	// ContainerExists controls the exit code of `podman container inspect`.
	ContainerExists bool
	// ContainerRunning controls the output of `podman inspect --format {{.State.Running}}`.
	ContainerRunning bool
	// SecretExists controls whether `podman secret inspect` succeeds.
	SecretExists bool
	// SecretExpiresAt is the value of the devsys.expires-at label on the
	// secret (YYYY-MM-DD). Empty means no expiry label is attached.
	SecretExpiresAt string
	// SecretValue is the output of `podman secret inspect --showsecret`.
	SecretValue string
	// VolumeExists controls the exit code of `podman volume inspect`.
	VolumeExists bool
	// ProjectPath is returned by `podman inspect --format {{range .Mounts}}...`
	// (used by getProjectPath). Empty causes getProjectPath to fail.
	ProjectPath string
	// BashExitCode is the exit code returned when `podman exec -it <name> bash`
	// is called.
	BashExitCode int
	// ActiveBashSessions is the number of bash lines reported by
	// `podman top <container> comm`. Defaults to 0 (no active sessions),
	// which causes runEnter to stop the container after the shell exits.
	ActiveBashSessions int
	// ImagePresent makes `podman image inspect` succeed (non-empty ID output).
	// When false the command exits non-zero, simulating a missing image.
	ImagePresent bool
	// ImageContainerfileHash is the value returned for the devsys.containerfile-hash
	// label when `podman image inspect --format '{{index .Config.Labels "..."}}' is
	// called. Empty string simulates an image built before the label was introduced.
	ImageContainerfileHash string
	// ImagePortsHash is the value returned for the devsys.ports-hash label.
	// Empty string simulates an image built before the label was introduced.
	ImagePortsHash string
	// ImageBaseVersion is the value returned for the devsys.base-version label.
	// Empty string simulates an image built before the label was introduced.
	ImageBaseVersion string
	// StartFails makes `podman start` exit non-zero (simulates a failed start).
	StartFails bool
	// StopFails makes `podman stop` exit non-zero (simulates a failed stop).
	StopFails bool
	// BuildFails makes `podman build` exit non-zero (simulates a build error).
	BuildFails bool
	// CreateFails makes `podman create` exit non-zero (simulates a create error).
	CreateFails bool
}

// Recorder reads the JSONL call log written by the fake subprocess and lets
// tests assert which podman invocations occurred.
type Recorder struct {
	// LogPath is the path of the JSONL call log. Each line is a JSON array of
	// the arguments passed to podman (excluding the "podman" binary name).
	LogPath string
}

// Calls returns every recorded podman invocation as a slice of argument
// slices. The first element of each inner slice is the subcommand (e.g. "rm").
// Useful when HasCall is too coarse or you need the exact arguments.
func (r *Recorder) Calls() [][]string {
	data, err := os.ReadFile(r.LogPath)
	if err != nil {
		return nil
	}
	var result [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			continue
		}
		result = append(result, args)
	}
	return result
}

// HasSubcommand reports whether any recorded call's first argument equals sub.
// More precise than HasCall when you want to assert on the subcommand only.
func (r *Recorder) HasSubcommand(sub string) bool {
	for _, call := range r.Calls() {
		if len(call) > 0 && call[0] == sub {
			return true
		}
	}
	return false
}

// HasCall reports whether any recorded call's argument list contains all the
// given substrings when joined with spaces.
// Example: rec.HasCall("start", "devsys-myproject") passes if any call had
// args like ["start", "devsys-myproject"].
func (r *Recorder) HasCall(fragments ...string) bool {
	data, err := os.ReadFile(r.LogPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			continue
		}
		joined := strings.Join(args, " ")
		ok := true
		for _, f := range fragments {
			if !strings.Contains(joined, f) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Install replaces podman.ExecCmd with the fake for the duration of t and
// returns a Recorder that captures which podman calls were made. It also
// restores the original ExecCmd and removes the call log on test cleanup.
//
// It also forces podman.DryRun to false for the duration of t, restoring
// whatever it was before on cleanup. Without this, a test binary compiled
// with `-tags dev` (internal/podman/dryrun_dev.go's init() defaults DryRun
// to true package-wide before any test runs) would silently skip every
// mutating podman call these tests exist to verify, since DryRun=true makes
// RunPodman/RunPodmanLive/etc. return early without ever reaching
// podman.ExecCmd — every rec.HasCall(...) assertion would then fail, not
// because behavior is wrong, but because the fake was never invoked. Fake
// tests are meant to verify real call construction regardless of which
// build tag compiled the test binary; DryRun itself is exercised
// separately and explicitly, by tests that set it back to true on purpose.
func Install(t *testing.T, opts Options) *Recorder {
	t.Helper()

	logFile, err := os.CreateTemp("", "podman-calls-*.jsonl")
	if err != nil {
		t.Fatalf("podmanfake: cannot create call log: %v", err)
	}
	logPath := logFile.Name()
	logFile.Close()

	origExecCmd := podman.ExecCmd
	origDryRun := podman.DryRun
	podman.DryRun = false
	t.Cleanup(func() {
		podman.ExecCmd = origExecCmd
		podman.DryRun = origDryRun
		os.Remove(logPath)
	})

	podman.ExecCmd = func(name string, args ...string) *exec.Cmd {
		// Re-invoke the current test binary as a subprocess. The subprocess
		// detects GO_WANT_FAKE_PODMAN=1 and dispatches on the podman args.
		subcmd := exec.Command(os.Args[0], append([]string{
			"-test.run=^TestFakePodman$",
			"--", name,
		}, args...)...)
		subcmd.Env = append(os.Environ(),
			"GO_WANT_FAKE_PODMAN=1",
			"FAKE_CALL_LOG="+logPath,
			fmt.Sprintf("FAKE_IMAGE_PRESENT=%v", opts.ImagePresent),
			"FAKE_IMAGE_CONTAINERFILE_HASH="+opts.ImageContainerfileHash,
			"FAKE_IMAGE_PORTS_HASH="+opts.ImagePortsHash,
			"FAKE_IMAGE_BASE_VERSION="+opts.ImageBaseVersion,
			fmt.Sprintf("FAKE_CONTAINER_EXISTS=%v", opts.ContainerExists),
			fmt.Sprintf("FAKE_CONTAINER_RUNNING=%v", opts.ContainerRunning),
			fmt.Sprintf("FAKE_SECRET_EXISTS=%v", opts.SecretExists),
			"FAKE_SECRET_EXPIRES_AT="+opts.SecretExpiresAt,
			"FAKE_SECRET_VALUE="+opts.SecretValue,
			fmt.Sprintf("FAKE_VOLUME_EXISTS=%v", opts.VolumeExists),
			"FAKE_PROJECT_PATH="+opts.ProjectPath,
			fmt.Sprintf("FAKE_BASH_EXIT=%d", opts.BashExitCode),
			fmt.Sprintf("FAKE_ACTIVE_BASH_SESSIONS=%d", opts.ActiveBashSessions),
			fmt.Sprintf("FAKE_START_FAILS=%v", opts.StartFails),
			fmt.Sprintf("FAKE_STOP_FAILS=%v", opts.StopFails),
			fmt.Sprintf("FAKE_BUILD_FAILS=%v", opts.BuildFails),
			fmt.Sprintf("FAKE_CREATE_FAILS=%v", opts.CreateFails),
		)
		return subcmd
	}

	return &Recorder{LogPath: logPath}
}

// Handle is called from TestFakePodman in each test package. When the process
// is running as a fake-podman subprocess (GO_WANT_FAKE_PODMAN=1), it records
// the call, writes canned output, and exits. Otherwise it is a no-op.
func Handle() {
	if os.Getenv("GO_WANT_FAKE_PODMAN") != "1" {
		return
	}

	// Find the "--" separator in os.Args. Everything after it is:
	//   [executable-name ("podman"), subcommand, args...]
	var podmanArgs []string
	for i, a := range os.Args {
		if a == "--" {
			podmanArgs = os.Args[i+1:]
			break
		}
	}

	// Strip the executable name ("podman") to get the actual arguments.
	var args []string
	if len(podmanArgs) > 1 {
		args = podmanArgs[1:]
	}

	// Append the call to the JSONL log so the Recorder can read it back.
	if logPath := os.Getenv("FAKE_CALL_LOG"); logPath != "" {
		entry, _ := json.Marshal(args)
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
			fmt.Fprintf(f, "%s\n", entry)
			f.Close()
		}
	}

	os.Exit(dispatch(args))
}

// envBool returns true if the env var named key equals "true".
func envBool(key string) bool { return os.Getenv(key) == "true" }

// dispatch maps podman subcommands to canned outputs and exit codes.
func dispatch(args []string) int {
	if len(args) == 0 {
		return 0
	}
	joined := strings.Join(args, " ")

	switch args[0] {

	// ---- image inspect (runDoctor base-image check, GetImageLabel) ----------
	case "image":
		if len(args) >= 2 && args[1] == "inspect" {
			if !envBool("FAKE_IMAGE_PRESENT") {
				fmt.Fprintln(os.Stderr, "image not found")
				return 1
			}
			// Label lookup: return just the label value.
			if strings.Contains(joined, "devsys.containerfile-hash") {
				fmt.Println(os.Getenv("FAKE_IMAGE_CONTAINERFILE_HASH"))
				return 0
			}
			if strings.Contains(joined, "devsys.ports-hash") {
				fmt.Println(os.Getenv("FAKE_IMAGE_PORTS_HASH"))
				return 0
			}
			if strings.Contains(joined, "devsys.base-version") {
				fmt.Println(os.Getenv("FAKE_IMAGE_BASE_VERSION"))
				return 0
			}
			fmt.Println("sha256:fakeimageid")
			return 0
		}
		return 0

	// ---- container inspect (ContainerExists) --------------------------------
	case "container":
		if len(args) >= 2 && args[1] == "inspect" {
			if envBool("FAKE_CONTAINER_EXISTS") {
				fmt.Println("[]")
				return 0
			}
			fmt.Fprintln(os.Stderr, "no such container")
			return 1
		}

	// ---- inspect (ContainerIsRunning, getProjectPath, ContainerExists) ------
	case "inspect":
		if strings.Contains(joined, ".State.Running") {
			if envBool("FAKE_CONTAINER_RUNNING") {
				fmt.Println("true")
			} else {
				fmt.Println("false")
			}
			return 0
		}
		if strings.Contains(joined, ".Mounts") {
			// getProjectPath template
			if p := os.Getenv("FAKE_PROJECT_PATH"); p != "" {
				fmt.Println(p)
				return 0
			}
			// empty output → getProjectPath returns "no mount found" error
			return 0
		}
		// Generic inspect — used by ContainerExists (some callers use plain inspect)
		if envBool("FAKE_CONTAINER_EXISTS") {
			fmt.Println("{}")
			return 0
		}
		fmt.Fprintln(os.Stderr, "no such container")
		return 1

	// ---- secret -------------------------------------------------------------
	case "secret":
		if len(args) < 2 {
			return 0
		}
		switch args[1] {
		case "inspect":
			if !envBool("FAKE_SECRET_EXISTS") {
				fmt.Fprintln(os.Stderr, "secret not found")
				return 1
			}
			if strings.Contains(joined, "--showsecret") {
				// GetSecretValue
				fmt.Println(os.Getenv("FAKE_SECRET_VALUE"))
				return 0
			}
			// GetSecretLabels / SecretExists — output JSON array
			expiresAt := os.Getenv("FAKE_SECRET_EXPIRES_AT")
			var labelsJSON string
			if expiresAt != "" {
				labelsJSON = fmt.Sprintf(`{"devsys":"true","devsys.expires-at":%q}`, expiresAt)
			} else {
				labelsJSON = `{"devsys":"true"}`
			}
			fmt.Printf("[{\"Spec\":{\"Labels\":%s}}]\n", labelsJSON)
			return 0
		case "create":
			return 0
		case "rm":
			return 0
		}

	// ---- volume -------------------------------------------------------------
	case "volume":
		if len(args) < 2 {
			return 0
		}
		switch args[1] {
		case "inspect":
			if envBool("FAKE_VOLUME_EXISTS") {
				fmt.Println("[]")
				return 0
			}
			fmt.Fprintln(os.Stderr, "no such volume")
			return 1
		case "create":
			return 0
		case "rm":
			return 0
		case "ls", "list":
			fmt.Println("[]")
			return 0
		}

	// ---- ps -----------------------------------------------------------------
	case "ps":
		fmt.Println("[]")
		return 0

	// ---- top (activeBashSessions) -------------------------------------------
	case "top":
		// Output: one header line + one "bash" line per active session.
		fmt.Println("COMMAND")
		n := 0
		fmt.Sscanf(os.Getenv("FAKE_ACTIVE_BASH_SESSIONS"), "%d", &n)
		for i := 0; i < n; i++ {
			fmt.Println("bash")
		}
		return 0

	// ---- simple lifecycle ---------------------------------------------------
	case "start":
		if envBool("FAKE_START_FAILS") {
			fmt.Fprintln(os.Stderr, "error starting container")
			return 1
		}
		return 0
	case "stop":
		if envBool("FAKE_STOP_FAILS") {
			fmt.Fprintln(os.Stderr, "error stopping container")
			return 1
		}
		return 0
	case "rm", "pull":
		return 0

	// ---- build / create -----------------------------------------------------
	case "build":
		if envBool("FAKE_BUILD_FAILS") {
			fmt.Fprintln(os.Stderr, "error building image")
			return 1
		}
		return 0
	case "create":
		if envBool("FAKE_CREATE_FAILS") {
			fmt.Fprintln(os.Stderr, "error creating container")
			return 1
		}
		return 0

	// ---- exec (interactive commands: bash, sh, claude) ----------------------
	case "exec":
		// args: exec -it <container> <cmd> [...]
		if len(args) >= 4 {
			switch args[3] {
			case "bash":
				bashExit := 0
				fmt.Sscanf(os.Getenv("FAKE_BASH_EXIT"), "%d", &bashExit)
				return bashExit
			}
		}
		return 0

	// ---- version ------------------------------------------------------------
	case "--version", "version":
		fmt.Println("podman version 99.0.0-fake")
		return 0
	}

	return 0
}
