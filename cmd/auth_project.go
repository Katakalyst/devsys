package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/katakalyst/devsys/internal/github"
	"github.com/katakalyst/devsys/internal/gitlab"
	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/workspace"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// authGitLabURL is the base URL a brand-new GitLab project is created
// against, or an attach target's project is looked up against when the user
// gives a bare owner/path instead of a full URL. Mirrors devsys init's own
// --gitlab-url flag (self-hosted instances supported).
var authGitLabURL string

// Scriptable-form flags (Phase 5, Git Remote & Credential Spec §9). Declared
// here alongside the code that reads them; registered on authCmd in
// cmd/auth.go's init(), same split already used for authGitLabURL above.
var (
	authScriptCreate bool
	authScriptAttach string
	authScriptRotate bool
	authScriptRemove bool
	authScriptName   string
	authScriptToken  string
	authScriptForce  bool
)

// tokenExpiryWarnDays is the same 30-day threshold devsys enter's
// checkTokenExpiry already uses — one shared threshold for the whole CLI,
// not a second number to keep in sync (Git Remote & Credential Spec §9).
const tokenExpiryWarnDays = 30

// repoAuthStatus is one discovered repo's current credential state, as shown
// in `devsys auth <project>`'s interactive listing (Git Remote & Credential
// Spec §8/§9).
type repoAuthStatus struct {
	Repo       workspace.Repo
	HasRemote  bool
	RemoteURL  string
	Platform   string // "" until a remote exists
	SecretName string // "" until a token has been minted/stored
	HasToken   bool
	ExpiresAt  string // "" when unknown (GitHub PATs never expose this)
}

// runAuthProject is authCmd's own RunE, invoked whenever the first argument
// to `devsys auth` doesn't match one of the claude/codex/gitlab/github
// bootstrap subcommands — i.e. it's a project name (Git Remote & Credential
// Spec §9's command list). Dispatches to the interactive listing (bare
// `devsys auth <project>`, no extra args, no operation flags) or the
// scriptable single-repo form (any extra positional arg, or any of
// --create/--attach/--rotate/--remove — Phase 5).
func runAuthProject(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devsys auth <project> | devsys auth gitlab|github|claude|codex")
	}
	projectName := args[0]
	extra := args[1:]

	operationFlagSet := authScriptCreate || authScriptAttach != "" || authScriptRotate || authScriptRemove
	if len(extra) == 0 && !operationFlagSet {
		return runAuthProjectInteractive(projectName)
	}
	return runAuthProjectScriptable(projectName, extra)
}

func runAuthProjectInteractive(projectName string) error {
	containerName := fmt.Sprintf("devsys-%s", projectName)
	if !podman.ContainerExists(containerName) {
		return fmt.Errorf("container %s does not exist — run 'devsys init' first", containerName)
	}
	workspaceRoot, err := getProjectPath(containerName)
	if err != nil {
		return fmt.Errorf("cannot determine project path for %s: %w", projectName, err)
	}

	reader := bufio.NewReader(os.Stdin)
	changed := false

	for {
		statuses, err := gatherRepoStatuses(projectName, workspaceRoot)
		if err != nil {
			return err
		}
		if len(statuses) == 0 {
			fmt.Println("No repos found in this project's workspace.")
			break
		}

		fmt.Println()
		printRepoListing(statuses)
		fmt.Print("\n  [1,2,...] select   [d] done\n\n> ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" || strings.EqualFold(line, "d") {
			break
		}

		indices, err := parseSelection(line, len(statuses))
		if err != nil {
			fmt.Printf("  %v\n", err)
			continue
		}
		for _, idx := range indices {
			didChange, err := configureRepo(reader, projectName, &statuses[idx])
			if err != nil {
				fmt.Printf("  Error: %v\n", err)
				continue
			}
			if didChange {
				changed = true
			}
		}
	}

	if !changed {
		return nil
	}
	return recreateContainerForAuth(projectName, true)
}

// --- Scriptable form (Phase 5) ----------------------------------------------

// runAuthProjectScriptable is the non-interactive counterpart to
// runAuthProjectInteractive: `devsys auth <project> [repo] [platform]
// --create/--attach/--rotate/--remove [--name] [--token] [--force]`
// (Git Remote & Credential Spec §9). Never prompts — every input it needs
// comes from a flag, and any missing requirement is a hard error.
func runAuthProjectScriptable(projectName string, extra []string) error {
	repoArg, platformArg, err := parseRepoAndPlatformArgs(extra)
	if err != nil {
		return err
	}

	// Validate flag combinations before touching Podman or the workspace at
	// all — a malformed invocation should fail on the actual mistake, not
	// on an unrelated "container doesn't exist" that happens to run first.
	ops := 0
	if authScriptCreate {
		ops++
	}
	if authScriptAttach != "" {
		ops++
	}
	if authScriptRotate {
		ops++
	}
	if authScriptRemove {
		ops++
	}
	if ops > 1 {
		return fmt.Errorf("only one of --create/--attach/--rotate/--remove may be given at a time")
	}

	// Spec §9's command syntax only lists [platform] on --create/--attach —
	// --rotate/--remove/bare always derive platform from the existing
	// remote. Rejecting it here rather than silently ignoring it catches a
	// real mistake (e.g. a typo'd or stale platform arg) instead of masking it.
	if platformArg != "" && !authScriptCreate && authScriptAttach == "" {
		return fmt.Errorf("platform is not accepted with --rotate/--remove/the bare form — it's always derived from the existing remote")
	}

	containerName := fmt.Sprintf("devsys-%s", projectName)
	if !podman.ContainerExists(containerName) {
		return fmt.Errorf("container %s does not exist — run 'devsys init' first", containerName)
	}
	workspaceRoot, err := getProjectPath(containerName)
	if err != nil {
		return fmt.Errorf("cannot determine project path for %s: %w", projectName, err)
	}

	statuses, err := gatherRepoStatuses(projectName, workspaceRoot)
	if err != nil {
		return err
	}
	if len(statuses) == 0 {
		return fmt.Errorf("no repos found in %s's workspace", projectName)
	}

	st, err := selectRepoForScriptable(statuses, repoArg)
	if err != nil {
		return err
	}

	changed, err := runScriptableOperation(projectName, st, platformArg)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	// Still confirm when there's an actual TTY to confirm against — the
	// spec's own reactive-auth flow has a human running this exact command
	// at a terminal, container often still running. Skip it only when
	// stdin genuinely isn't interactive (true unattended automation),
	// where blocking on a confirmation would just hang forever.
	return recreateContainerForAuth(projectName, isStdinInteractive())
}

// isStdinInteractive reports whether stdin is an actual terminal, not a
// pipe/redirect/closed fd. Used only to decide whether the scriptable
// form's container-recreation step can safely ask for confirmation — the
// interactive listing form always passes true directly, since it's already
// mid-conversation on stdin by definition.
func isStdinInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// parseRepoAndPlatformArgs classifies devsys auth <project>'s remaining
// positional args into repo and platform: an arg equal to "gitlab" or
// "github" is the platform, anything else is the repo (Spec §9's
// "<project> [repo] [platform]" — order-independent here so repo can be
// omitted without an awkward placeholder when only platform is needed). A
// repo whose own RelPath happens to literally be "gitlab"/"github" can't be
// addressed this way — an accepted, narrow edge case, same category as the
// project-name-vs-bootstrap-subcommand-name ambiguity already accepted
// elsewhere on this command.
func parseRepoAndPlatformArgs(extra []string) (repo, platform string, err error) {
	if len(extra) > 2 {
		return "", "", fmt.Errorf("too many arguments — expected [repo] [platform]")
	}
	for _, a := range extra {
		lower := strings.ToLower(a)
		if lower == "gitlab" || lower == "github" {
			if platform != "" {
				return "", "", fmt.Errorf("platform given twice")
			}
			platform = lower
			continue
		}
		if repo != "" {
			return "", "", fmt.Errorf("unexpected extra argument %q", a)
		}
		repo = a
	}
	return repo, platform, nil
}

// selectRepoForScriptable resolves which discovered repo a scriptable
// invocation applies to: the explicitly named one, or the project's only
// one if none was given. Spec §9's stated error case: "[repo] omitted with
// 2+ repos present -> error listing the actual discovered paths, not a guess."
func selectRepoForScriptable(statuses []repoAuthStatus, repoArg string) (*repoAuthStatus, error) {
	if repoArg != "" {
		for i := range statuses {
			if statuses[i].Repo.RelPath == repoArg {
				return &statuses[i], nil
			}
		}
		return nil, fmt.Errorf("no repo at %q — discovered repos: %s", repoArg, joinRepoPaths(statuses))
	}
	if len(statuses) != 1 {
		return nil, fmt.Errorf("project has more than one repo — specify which one: %s", joinRepoPaths(statuses))
	}
	return &statuses[0], nil
}

func joinRepoPaths(statuses []repoAuthStatus) string {
	paths := make([]string, len(statuses))
	for i, st := range statuses {
		paths[i] = st.Repo.RelPath
	}
	return strings.Join(paths, ", ")
}

// runScriptableOperation dispatches on which operation flag was given —
// exactly mirroring configureRepo's dispatch, but flag-driven instead of
// menu-driven, and reports whether anything changed the same way.
func runScriptableOperation(projectName string, st *repoAuthStatus, platformArg string) (bool, error) {
	switch {
	case authScriptCreate:
		return scriptableCreateOrAttach(projectName, st, "create", authScriptName, platformArg)
	case authScriptAttach != "":
		return scriptableCreateOrAttach(projectName, st, "attach", authScriptAttach, platformArg)
	case authScriptRotate:
		return scriptableRotate(st)
	case authScriptRemove:
		return scriptableRemove(st)
	default:
		return scriptableEnsure(projectName, st)
	}
}

// scriptableCreateOrAttach is --create/--attach's scriptable form —
// the flag-driven counterpart to configureNoRemote/changeRepoTarget.
// Refuses an already-configured repo unless --force is given, matching the
// interactive [c] Change menu's underlying operation (Spec §9: "--force is
// how the scriptable form does what the interactive menu's [c] Change does").
func scriptableCreateOrAttach(projectName string, st *repoAuthStatus, mode, nameOrTarget, platformArg string) (bool, error) {
	if st.HasRemote {
		if !authScriptForce {
			return false, fmt.Errorf("%q is already configured — use --force to replace, --remove to clear, or --rotate to refresh", st.Repo.RelPath)
		}
		if err := revokeAndDeleteSecret(st); err != nil {
			return false, err
		}
		if err := clearOriginRemote(st.Repo.AbsPath); err != nil {
			return false, err
		}
		st.HasRemote, st.HasToken = false, false
		st.RemoteURL, st.Platform, st.SecretName, st.ExpiresAt = "", "", "", ""
	}

	if platformArg == "" {
		return false, fmt.Errorf("platform is required for --create/--attach on a repo with no remote — pass gitlab or github")
	}

	var remoteURL, tokenValue string
	var labels map[string]string
	var err error
	switch platformArg {
	case "gitlab":
		remoteURL, tokenValue, labels, err = setupGitLabRemoteScriptable(projectName, st, mode, nameOrTarget)
	case "github":
		remoteURL, tokenValue, err = setupGitHubRemoteScriptable(projectName, st, mode, nameOrTarget, authScriptToken)
	default:
		return false, fmt.Errorf("unknown platform %q — must be gitlab or github", platformArg)
	}
	if err != nil {
		return false, err
	}

	if err := writeOriginRemote(st.Repo.AbsPath, remoteURL); err != nil {
		return false, fmt.Errorf("cannot write remote: %w", err)
	}
	repoID, err := workspace.RepoIDFromURL(remoteURL)
	if err != nil {
		return false, fmt.Errorf("cannot determine repo id: %w", err)
	}
	secretName := workspace.SecretName(projectName, repoID, platformArg)
	if err := storeRepoSecret(secretName, tokenValue, labels); err != nil {
		return false, err
	}

	st.HasRemote, st.RemoteURL, st.Platform, st.SecretName, st.HasToken = true, remoteURL, platformArg, secretName, true
	st.ExpiresAt = labels["devsys.expires-at"]
	fmt.Printf("%s: remote wired, token stored (%s)\n", st.Repo.RelPath, secretName)
	return true, nil
}

// scriptableRotate is --rotate's scriptable form — rotateRepoToken's logic
// without the confirmation prompt (scriptable never prompts) and reading
// the GitHub PAT from --token instead of stdin.
func scriptableRotate(st *repoAuthStatus) (bool, error) {
	if !st.HasToken {
		return false, fmt.Errorf("%q has no token to rotate — use --create or --attach first", st.Repo.RelPath)
	}

	var tokenValue, userInfo string
	var labels map[string]string

	switch st.Platform {
	case "gitlab":
		if err := revokeGitLabRepoToken(st.RemoteURL); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not revoke old GitLab token via API: %v\n", err)
		}
		var err error
		tokenValue, labels, err = mintGitLabTokenForRemote(st.RemoteURL)
		if err != nil {
			return false, err
		}
		userInfo = "oauth2"
	case "github":
		if authScriptToken == "" {
			return false, fmt.Errorf("--token is required to rotate a GitHub PAT in non-interactive mode")
		}
		repoPath, err := workspace.PathFromURL(st.RemoteURL)
		if err != nil {
			return false, fmt.Errorf("cannot determine owner/repo: %w", err)
		}
		if err := verifyGitHubPAT(repoPath, authScriptToken); err != nil {
			return false, fmt.Errorf("cannot verify %s with this PAT: %w", repoPath, err)
		}
		tokenValue = authScriptToken
		userInfo = "x-access-token"
	}

	if err := storeRepoSecret(st.SecretName, tokenValue, labels); err != nil {
		return false, err
	}
	st.ExpiresAt = labels["devsys.expires-at"]
	st.HasToken = true

	if remoteHasEmbeddedCredentials(st.RemoteURL) {
		newRemoteURL, err := embedTokenInHTTPSURL(st.RemoteURL, userInfo, tokenValue)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not rewrite remote URL with the new token: %v\n", err)
		} else if err := writeOriginRemote(st.Repo.AbsPath, newRemoteURL); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not update the remote with the new token: %v\n", err)
		} else {
			st.RemoteURL = newRemoteURL
		}
	}
	fmt.Printf("%s: new token stored\n", st.Repo.RelPath)
	return true, nil
}

// scriptableRemove is --remove's scriptable form (R12.5): revoke and
// unmount, remote left as-is. A repo with no token is a no-op, not an
// error — nothing to remove.
func scriptableRemove(st *repoAuthStatus) (bool, error) {
	if !st.HasToken {
		fmt.Printf("%s: no token to remove\n", st.Repo.RelPath)
		return false, nil
	}
	if err := revokeAndDeleteSecret(st); err != nil {
		return false, err
	}
	st.HasToken, st.SecretName, st.ExpiresAt = false, "", ""
	fmt.Printf("%s: token removed, remote left as-is\n", st.Repo.RelPath)
	return true, nil
}

// scriptableEnsure is the bare scriptable form's operation (no --create/
// --attach/--rotate/--remove given): R11 "ensure/refresh" — confirm a
// credential is present and working, safe to call whether or not one
// already exists, never implying a discard of one that's still fine. Spec
// §9: "devsys auth <project> [repo] # bare — only valid if repo already has
// a remote."
func scriptableEnsure(projectName string, st *repoAuthStatus) (bool, error) {
	switch {
	case !st.HasRemote:
		return false, fmt.Errorf("%q has no remote — use --create or --attach", st.Repo.RelPath)
	case !st.HasToken:
		return scriptableEnsureToken(projectName, st)
	default:
		fmt.Printf("%s: already configured — %s, %s\n", st.Repo.RelPath, st.Platform, tokenExpiryText(st.ExpiresAt))
		return false, nil
	}
}

func scriptableEnsureToken(projectName string, st *repoAuthStatus) (bool, error) {
	repoID, err := workspace.RepoIDFromURL(st.RemoteURL)
	if err != nil {
		return false, fmt.Errorf("cannot determine repo id: %w", err)
	}
	secretName := workspace.SecretName(projectName, repoID, st.Platform)

	var tokenValue string
	var labels map[string]string
	switch st.Platform {
	case "gitlab":
		tokenValue, labels, err = mintGitLabTokenForRemote(st.RemoteURL)
	case "github":
		if authScriptToken == "" {
			return false, fmt.Errorf("--token is required for GitHub in non-interactive mode")
		}
		repoPath, pathErr := workspace.PathFromURL(st.RemoteURL)
		if pathErr != nil {
			return false, fmt.Errorf("cannot determine owner/repo: %w", pathErr)
		}
		if verifyErr := verifyGitHubPAT(repoPath, authScriptToken); verifyErr != nil {
			return false, fmt.Errorf("cannot verify %s with this PAT: %w", repoPath, verifyErr)
		}
		tokenValue = authScriptToken
	}
	if err != nil {
		return false, err
	}

	if err := storeRepoSecret(secretName, tokenValue, labels); err != nil {
		return false, err
	}
	st.SecretName, st.HasToken = secretName, true
	st.ExpiresAt = labels["devsys.expires-at"]
	fmt.Printf("%s: token stored (%s)\n", st.Repo.RelPath, secretName)
	return true, nil
}

// gatherRepoStatuses runs the recursive discovery walk and derives each
// repo's current credential state — purely by reading git config and
// checking which Podman secrets already exist, never from a stored config
// file (R15).
func gatherRepoStatuses(projectName, workspaceRoot string) ([]repoAuthStatus, error) {
	repos, err := workspace.DiscoverRepos(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot scan workspace for repos: %w", err)
	}

	statuses := make([]repoAuthStatus, 0, len(repos))
	for _, r := range repos {
		st := repoAuthStatus{Repo: r}

		originURL, err := r.OriginURL()
		if err != nil {
			if errors.Is(err, workspace.ErrNoOrigin) {
				statuses = append(statuses, st)
				continue
			}
			return nil, fmt.Errorf("cannot read remote for %s: %w", r.RelPath, err)
		}
		st.HasRemote = true
		st.RemoteURL = originURL

		platform, err := workspace.PlatformFromURL(originURL)
		if err != nil {
			return nil, fmt.Errorf("cannot determine platform for %s: %w", r.RelPath, err)
		}
		st.Platform = platform

		repoID, err := workspace.RepoIDFromURL(originURL)
		if err != nil {
			return nil, fmt.Errorf("cannot determine repo id for %s: %w", r.RelPath, err)
		}
		secretName := workspace.SecretName(projectName, repoID, platform)
		if podman.SecretExists(secretName) {
			st.SecretName = secretName
			st.HasToken = true
			if labels, err := podman.GetSecretLabels(secretName); err == nil {
				st.ExpiresAt = labels["devsys.expires-at"]
			}
		}

		statuses = append(statuses, st)
	}
	return statuses, nil
}

func printRepoListing(statuses []repoAuthStatus) {
	for i, st := range statuses {
		switch {
		case !st.HasRemote:
			fmt.Printf("  %d. %-15s no remote\n", i+1, st.Repo.RelPath)
		case !st.HasToken:
			fmt.Printf("  %d. %-15s %-7s no token\n", i+1, st.Repo.RelPath, st.Platform)
		default:
			fmt.Printf("  %d. %-15s %-7s %s\n", i+1, st.Repo.RelPath, st.Platform, tokenExpiryText(st.ExpiresAt))
		}
	}
}

// tokenExpiryText formats a token's remaining lifetime, or "token ok" when
// the platform doesn't expose an expiry (GitHub fine-grained PATs — Git
// Remote & Credential Implementation Plan.md's open question on this is
// resolved here: no expiry is shown when none is known, rather than guessing).
func tokenExpiryText(expiresAtStr string) string {
	if expiresAtStr == "" {
		return "token ok"
	}
	expiresAt, err := time.Parse("2006-01-02", expiresAtStr)
	if err != nil {
		return "token ok"
	}
	daysLeft := int(time.Until(expiresAt).Hours() / 24)
	warn := ""
	if daysLeft <= tokenExpiryWarnDays {
		warn = "   ⚠"
	}
	return fmt.Sprintf("token ok, expires in %d days%s", daysLeft, warn)
}

// parseSelection parses a multi-select line of 1-based indices, separated by
// comma or space — never inferred from concatenated digits ("15" is index
// fifteen, never "1" and "5") (Git Remote & Credential Spec §9). Duplicate
// indices are deduplicated, first-occurrence order preserved.
func parseSelection(input string, max int) ([]int, error) {
	fields := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == ' '
	})
	var indices []int
	seen := make(map[int]bool)
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("invalid selection %q", f)
		}
		if n < 1 || n > max {
			return nil, fmt.Errorf("selection %d out of range (1-%d)", n, max)
		}
		if !seen[n] {
			seen[n] = true
			indices = append(indices, n-1)
		}
	}
	if len(indices) == 0 {
		return nil, fmt.Errorf("no selection")
	}
	return indices, nil
}

// configureRepo dispatches on repo state and reports whether it actually
// changed anything — distinct from erroring, since e.g. opening the
// already-configured menu and choosing [b] Back is neither an error nor a
// change, and must not trigger the caller's end-of-session container
// recreation (Git Remote & Credential Spec §7: recreation is a real
// interruption, not to be triggered needlessly).
func configureRepo(reader *bufio.Reader, projectName string, st *repoAuthStatus) (bool, error) {
	switch {
	case !st.HasRemote:
		return configureNoRemote(reader, projectName, st)
	case !st.HasToken:
		return configureRemoteNoToken(reader, projectName, st)
	default:
		return configureAlreadySet(reader, projectName, st)
	}
}

// configureNoRemote runs the create-or-attach wizard for a repo with no
// remote yet — the only branch that ever writes a remote (Git Remote &
// Credential Spec §7: "auth never rewrites a remote it didn't create").
func configureNoRemote(reader *bufio.Reader, projectName string, st *repoAuthStatus) (bool, error) {
	fmt.Printf("Configuring %q (no remote):\n", st.Repo.RelPath)
	platform, err := promptPlatform(reader)
	if err != nil {
		return false, err
	}
	mode, err := promptCreateOrAttach(reader)
	if err != nil {
		return false, err
	}

	var remoteURL, tokenValue string
	var labels map[string]string
	switch platform {
	case "gitlab":
		remoteURL, tokenValue, labels, err = setupGitLabRemote(reader, projectName, st, mode)
	case "github":
		remoteURL, tokenValue, err = setupGitHubRemote(reader, projectName, st, mode)
	}
	if err != nil {
		return false, err
	}

	if err := writeOriginRemote(st.Repo.AbsPath, remoteURL); err != nil {
		return false, fmt.Errorf("cannot write remote: %w", err)
	}

	repoID, err := workspace.RepoIDFromURL(remoteURL)
	if err != nil {
		return false, fmt.Errorf("cannot determine repo id: %w", err)
	}
	secretName := workspace.SecretName(projectName, repoID, platform)
	if err := storeRepoSecret(secretName, tokenValue, labels); err != nil {
		return false, err
	}

	fmt.Printf("  -> remote wired, token stored (%s)\n", secretName)
	st.HasRemote = true
	st.RemoteURL = remoteURL
	st.Platform = platform
	st.SecretName = secretName
	st.HasToken = true
	st.ExpiresAt = labels["devsys.expires-at"]
	return true, nil
}

// configureRemoteNoToken mints/stores a token for a repo whose remote the
// agent already set itself. Platform is derived from the existing URL; no
// create/attach question, no remote rewrite (Git Remote & Credential Spec §7/§8).
func configureRemoteNoToken(reader *bufio.Reader, projectName string, st *repoAuthStatus) (bool, error) {
	fmt.Printf("Configuring %q: remote already set -> %s\n", st.Repo.RelPath, st.RemoteURL)
	fmt.Printf("  Platform: %s (derived from remote)\n", st.Platform)

	repoID, err := workspace.RepoIDFromURL(st.RemoteURL)
	if err != nil {
		return false, fmt.Errorf("cannot determine repo id: %w", err)
	}
	secretName := workspace.SecretName(projectName, repoID, st.Platform)

	var tokenValue string
	var labels map[string]string
	switch st.Platform {
	case "gitlab":
		tokenValue, labels, err = mintGitLabTokenForRemote(st.RemoteURL)
	case "github":
		repoPath, pathErr := workspace.PathFromURL(st.RemoteURL)
		if pathErr != nil {
			return false, fmt.Errorf("cannot determine owner/repo: %w", pathErr)
		}
		tokenValue, err = promptAndVerifyGitHubPAT(reader, repoPath)
	}
	if err != nil {
		return false, err
	}

	if err := storeRepoSecret(secretName, tokenValue, labels); err != nil {
		return false, err
	}
	fmt.Printf("  -> token stored (%s)\n", secretName)
	st.SecretName = secretName
	st.HasToken = true
	st.ExpiresAt = labels["devsys.expires-at"]
	return true, nil
}

// configureAlreadySet shows the change/rotate/remove/back menu for a fully
// configured repo (Git Remote & Credential Spec §9) — selecting one is never
// a no-op or an error, though only some choices actually change anything.
func configureAlreadySet(reader *bufio.Reader, projectName string, st *repoAuthStatus) (bool, error) {
	fmt.Printf("%q is already configured — %s, %s.\n", st.Repo.RelPath, st.Platform, tokenExpiryText(st.ExpiresAt))
	fmt.Println("  [c] Change platform/target")
	fmt.Println("  [x] Rotate token (same target, fresh token)")
	fmt.Println("  [r] Remove token (remote left as-is)")
	fmt.Println("  [b] Back")
	fmt.Print("\n> ")
	line, _ := reader.ReadString('\n')
	choice := strings.ToLower(strings.TrimSpace(line))

	switch choice {
	case "c":
		return changeRepoTarget(reader, projectName, st)
	case "x":
		return rotateRepoToken(reader, st)
	case "r":
		return removeRepoToken(st)
	case "b", "":
		return false, nil
	default:
		fmt.Println("  Unrecognized option.")
		return false, nil
	}
}

// changeRepoTarget is the [c] menu option: revoke/delete the current
// credential, clear the remote, and re-run the create/attach wizard — an
// explicit, user-requested rewrite. Doesn't conflict with "auth never
// rewrites a remote it didn't create" (Spec §7): that rule is about auth's
// own default behavior, not a menu the user deliberately opened (Spec §9).
func changeRepoTarget(reader *bufio.Reader, projectName string, st *repoAuthStatus) (bool, error) {
	if err := revokeAndDeleteSecret(st); err != nil {
		return false, err
	}
	if err := clearOriginRemote(st.Repo.AbsPath); err != nil {
		return false, err
	}
	st.HasRemote = false
	st.HasToken = false
	st.RemoteURL = ""
	st.Platform = ""
	st.SecretName = ""
	st.ExpiresAt = ""
	return configureNoRemote(reader, projectName, st)
}

// rotateRepoToken is the [x] menu option: same target, fresh token value.
// GitLab is fully automatic (revoke via API, mint a new one). GitHub fine-
// grained PATs have no revoke/remint API (Git Remote & Credential
// Implementation Plan.md's open question on this — resolved here: prompt
// for a newly user-created PAT, same as the remote-no-token flow, with a
// reminder to revoke the old one manually).
func rotateRepoToken(reader *bufio.Reader, st *repoAuthStatus) (bool, error) {
	fmt.Printf("This will revoke the current token and mint a fresh one for the same target (%s).\n", st.RemoteURL)
	if !confirm(reader, "Continue?") {
		fmt.Println("  Skipped.")
		return false, nil
	}

	var tokenValue, userInfo string
	var labels map[string]string

	switch st.Platform {
	case "gitlab":
		if err := revokeGitLabRepoToken(st.RemoteURL); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not revoke old GitLab token via API: %v\n", err)
		}
		var err error
		tokenValue, labels, err = mintGitLabTokenForRemote(st.RemoteURL)
		if err != nil {
			return false, err
		}
		userInfo = "oauth2"
		if err := storeRepoSecret(st.SecretName, tokenValue, labels); err != nil {
			return false, err
		}
		st.ExpiresAt = labels["devsys.expires-at"]
		fmt.Printf("  -> new token minted (expires %s)\n", st.ExpiresAt)
	case "github":
		fmt.Println("  GitHub fine-grained PATs can't be rotated via API — create a new one, then paste it below.")
		fmt.Println("  Remember to revoke the old PAT yourself at github.com/settings/tokens once the new one is confirmed working.")
		repoPath, err := workspace.PathFromURL(st.RemoteURL)
		if err != nil {
			return false, fmt.Errorf("cannot determine owner/repo: %w", err)
		}
		tokenValue, err = promptAndVerifyGitHubPAT(reader, repoPath)
		if err != nil {
			return false, err
		}
		userInfo = "x-access-token"
		if err := storeRepoSecret(st.SecretName, tokenValue, nil); err != nil {
			return false, err
		}
		st.ExpiresAt = ""
		fmt.Println("  -> new token stored")
	}
	st.HasToken = true

	// If the remote's URL has the old token embedded (the case whenever auth
	// itself wrote it), plain `git push`/`pull` breaks silently the moment
	// the old token is revoked unless the URL is rewritten too (Git Remote &
	// Credential Spec §9's explicit "real mechanical detail" for rotate). A
	// remote auth didn't write (no embedded userinfo) is left untouched,
	// consistent with auth never rewriting a remote it didn't create.
	if remoteHasEmbeddedCredentials(st.RemoteURL) {
		newRemoteURL, err := embedTokenInHTTPSURL(st.RemoteURL, userInfo, tokenValue)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not rewrite remote URL with the new token: %v\n", err)
		} else if err := writeOriginRemote(st.Repo.AbsPath, newRemoteURL); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not update the remote with the new token: %v\n", err)
		} else {
			st.RemoteURL = newRemoteURL
			fmt.Println("  -> remote URL updated with the new token")
		}
	}
	return true, nil
}

// remoteHasEmbeddedCredentials reports whether remoteURL already carries
// userinfo (user:token@host) — the signal that auth itself wrote this URL
// (setupGitLabRemote/setupGitHubRemote always embed a token), as opposed to
// a bare URL the agent set itself via plain `git remote add`, which auth
// never rewrites.
func remoteHasEmbeddedCredentials(remoteURL string) bool {
	u, err := url.Parse(remoteURL)
	if err != nil {
		return false
	}
	return u.User != nil
}

// removeRepoToken is the [r] menu option: revoke and unmount, remote left
// as-is (R12.5).
func removeRepoToken(st *repoAuthStatus) (bool, error) {
	if err := revokeAndDeleteSecret(st); err != nil {
		return false, err
	}
	st.HasToken = false
	st.SecretName = ""
	st.ExpiresAt = ""
	fmt.Println("  -> token removed, remote left as-is")
	return true, nil
}

func revokeAndDeleteSecret(st *repoAuthStatus) error {
	switch st.Platform {
	case "gitlab":
		if err := revokeGitLabRepoToken(st.RemoteURL); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not revoke GitLab token via API: %v\n", err)
		}
	case "github":
		fmt.Println("  Remember to revoke the old fine-grained PAT yourself at github.com/settings/tokens.")
	}
	if st.SecretName != "" && podman.SecretExists(st.SecretName) {
		if err := podman.DeleteSecret(st.SecretName); err != nil {
			return fmt.Errorf("cannot delete secret %s: %w", st.SecretName, err)
		}
	}
	return nil
}

// --- Prompts ---------------------------------------------------------------

func promptPlatform(reader *bufio.Reader) (string, error) {
	for {
		fmt.Print("  Platform? [gitlab/github]: ")
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("cannot read input: %w", err)
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "gitlab" || line == "github" {
			return line, nil
		}
		fmt.Println("  Please answer 'gitlab' or 'github'.")
	}
}

func promptCreateOrAttach(reader *bufio.Reader) (string, error) {
	for {
		fmt.Print("  Create a new project, or attach to an existing one? [create/attach]: ")
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("cannot read input: %w", err)
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "create" || line == "attach" {
			return line, nil
		}
		fmt.Println("  Please answer 'create' or 'attach'.")
	}
}

// promptAndVerifyGitHubPAT collects a fine-grained PAT for a specific
// owner/repo and verifies it against that repo before accepting it — this
// is what surfaces a REST error directly for a typo'd --attach target or a
// PAT with the wrong scope (Spec §9's error cases), since GitHub gives
// devsys no other way to validate either one ahead of time.
func promptAndVerifyGitHubPAT(reader *bufio.Reader, ownerRepo string) (string, error) {
	fmt.Printf("  Create a fine-grained PAT scoped to %s at github.com/settings/tokens?type=beta\n", ownerRepo)
	fmt.Println("  Repository permissions needed (Metadata: Read-only is auto-selected with these):")
	fmt.Println("    Contents:      Read and write  (git push/pull, releases, tags)")
	fmt.Println("    Issues:        Read and write  (issues, comments, milestones)")
	fmt.Println("    Pull requests: Read and write  (create, review, merge)")
	fmt.Println("    Actions:       Read-only        (CI status and logs)")
	fmt.Println("  Do not grant Administration — that's repo settings/collaborators, not covered by any devsys workflow.")
	fmt.Print("  Enter PAT: ")
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("cannot read PAT: %w", err)
	}
	pat := strings.TrimSpace(line)
	if pat == "" {
		return "", fmt.Errorf("PAT cannot be empty")
	}
	if err := verifyGitHubPAT(ownerRepo, pat); err != nil {
		return "", fmt.Errorf("cannot verify %s with this PAT: %w", ownerRepo, err)
	}
	return pat, nil
}

// verifyGitHubPAT checks a fine-grained PAT against a specific owner/repo —
// the shared core of promptAndVerifyGitHubPAT (interactive) and the
// scriptable GitHub paths, which get the PAT from --token instead of a
// prompt but still need the same verification (Spec §9's error cases).
func verifyGitHubPAT(ownerRepo, token string) error {
	owner, repo, ok := strings.Cut(ownerRepo, "/")
	if !ok {
		return fmt.Errorf("expected owner/repo, got %q", ownerRepo)
	}
	return github.NewClient(token).GetRepo(owner, repo)
}

// --- GitLab-specific setup --------------------------------------------------

// gitLabClient builds a GitLab client from the bootstrap PAT. Shared by
// every GitLab code path so failure behaves identically everywhere:
// returning an error here, rather than prompting, is what makes the
// scriptable form's "bootstrap PAT missing -> fails immediately, never
// prompts" rule (Spec §9) automatic rather than something each caller has
// to remember to implement.
func gitLabClient() (*gitlab.Client, error) {
	bootstrapPAT, err := podman.GetSecretValue("devsys-bootstrap-gitlab-token")
	if err != nil {
		return nil, fmt.Errorf("cannot read bootstrap GitLab PAT (run 'devsys auth gitlab' first): %w", err)
	}
	return gitlab.NewClient(authGitLabURL, bootstrapPAT), nil
}

// resolveGitLabProject creates ("create" mode, nameOrTarget is the new
// project's name) or looks up ("attach" mode, nameOrTarget is an
// owner/repo or full URL) a GitLab project. Shared by the interactive and
// scriptable forms — the only difference between them is how nameOrTarget
// gets collected (prompted vs. --name/--attach flags).
func resolveGitLabProject(glClient *gitlab.Client, mode, nameOrTarget string) (projectID int, webURL string, err error) {
	if mode == "create" {
		projectID, webURL, err = glClient.CreateProject(nameOrTarget)
		if err != nil {
			return 0, "", fmt.Errorf("cannot create GitLab project: %w", err)
		}
		return projectID, webURL, nil
	}
	projectID, err = glClient.GetProject(nameOrTarget)
	if err != nil {
		return 0, "", fmt.Errorf("cannot find GitLab project %s: %w", nameOrTarget, err)
	}
	if strings.HasPrefix(nameOrTarget, "http://") || strings.HasPrefix(nameOrTarget, "https://") {
		webURL = nameOrTarget
	} else {
		webURL = strings.TrimRight(authGitLabURL, "/") + "/" + nameOrTarget
	}
	return projectID, webURL, nil
}

func setupGitLabRemote(reader *bufio.Reader, projectName string, st *repoAuthStatus, mode string) (remoteURL, tokenValue string, labels map[string]string, err error) {
	glClient, err := gitLabClient()
	if err != nil {
		return "", "", nil, err
	}

	var nameOrTarget string
	if mode == "create" {
		defaultName := repoDefaultName(projectName, st.Repo.RelPath)
		fmt.Printf("  Project name [%s]: ", defaultName)
		line, _ := reader.ReadString('\n')
		name := strings.TrimSpace(line)
		if name == "" {
			name = defaultName
		}
		nameOrTarget = name
	} else {
		fmt.Print("  GitLab project (owner/repo or URL): ")
		line, _ := reader.ReadString('\n')
		target := strings.TrimSpace(line)
		if target == "" {
			return "", "", nil, fmt.Errorf("GitLab project target cannot be empty")
		}
		nameOrTarget = target
	}

	projectID, webURL, err := resolveGitLabProject(glClient, mode, nameOrTarget)
	if err != nil {
		return "", "", nil, err
	}
	if mode == "create" {
		fmt.Printf("  -> created %s\n", webURL)
	}

	tokenValue, labels, err = mintGitLabToken(glClient, projectID, webURL)
	if err != nil {
		return "", "", nil, err
	}
	remoteURL, err = embedTokenInHTTPSURL(webURL, "oauth2", tokenValue)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot build remote URL: %w", err)
	}
	return remoteURL, tokenValue, labels, nil
}

// setupGitLabRemoteScriptable is setupGitLabRemote's non-interactive
// counterpart: nameOrTarget comes from --name/--attach, never prompted
// (Git Remote & Credential Spec §9: the scriptable form never prompts).
func setupGitLabRemoteScriptable(projectName string, st *repoAuthStatus, mode, nameOrTarget string) (remoteURL, tokenValue string, labels map[string]string, err error) {
	glClient, err := gitLabClient()
	if err != nil {
		return "", "", nil, err
	}
	if mode == "create" && nameOrTarget == "" {
		nameOrTarget = repoDefaultName(projectName, st.Repo.RelPath)
	}
	if mode == "attach" && nameOrTarget == "" {
		return "", "", nil, fmt.Errorf("--attach requires a target (owner/repo or URL)")
	}

	projectID, webURL, err := resolveGitLabProject(glClient, mode, nameOrTarget)
	if err != nil {
		return "", "", nil, err
	}
	tokenValue, labels, err = mintGitLabToken(glClient, projectID, webURL)
	if err != nil {
		return "", "", nil, err
	}
	remoteURL, err = embedTokenInHTTPSURL(webURL, "oauth2", tokenValue)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot build remote URL: %w", err)
	}
	return remoteURL, tokenValue, labels, nil
}

// mintGitLabTokenForRemote mints a fresh project access token for whatever
// GitLab project remoteURL (bare or token-embedded) points at, looking up
// the project ID first. Used by the remote-already-exists and rotate
// branches, which only have a URL, not an ID already in hand.
func mintGitLabTokenForRemote(remoteURL string) (tokenValue string, labels map[string]string, err error) {
	glClient, err := gitLabClient()
	if err != nil {
		return "", nil, err
	}
	projectID, err := glClient.GetProject(remoteURL)
	if err != nil {
		return "", nil, fmt.Errorf("cannot find GitLab project for %s: %w", remoteURL, err)
	}
	return mintGitLabToken(glClient, projectID, remoteURL)
}

// mintGitLabToken mints a fresh project access token for an already-known
// projectID, named deterministically from repoURL's repo-id so
// revokeGitLabRepoToken can find it again later by name alone.
func mintGitLabToken(glClient *gitlab.Client, projectID int, repoURL string) (tokenValue string, labels map[string]string, err error) {
	repoID, err := workspace.RepoIDFromURL(repoURL)
	if err != nil {
		return "", nil, fmt.Errorf("cannot determine repo id: %w", err)
	}
	tokenName := fmt.Sprintf("devsys-%s-token", repoID)
	expiresAt := time.Now().AddDate(0, 0, 90).Format("2006-01-02")

	_, tokenValue, err = glClient.CreateProjectToken(projectID, tokenName, []string{"api"}, 40, expiresAt)
	if err != nil {
		return "", nil, fmt.Errorf("cannot mint GitLab token: %w", err)
	}
	return tokenValue, map[string]string{"devsys": "true", "devsys.expires-at": expiresAt}, nil
}

// revokeGitLabRepoToken best-effort revokes the devsys-minted token for
// remoteURL, matched by the deterministic name mintGitLabTokenForRemote
// gives it. Finding nothing matching is not an error — there may be
// nothing to revoke (e.g. an agent-set remote whose token was minted before
// this naming convention, or already revoked).
func revokeGitLabRepoToken(remoteURL string) error {
	glClient, err := gitLabClient()
	if err != nil {
		return err
	}
	projectID, err := glClient.GetProject(remoteURL)
	if err != nil {
		return err
	}
	repoID, err := workspace.RepoIDFromURL(remoteURL)
	if err != nil {
		return err
	}
	wantName := fmt.Sprintf("devsys-%s-token", repoID)

	tokens, err := glClient.GetProjectTokens(projectID)
	if err != nil {
		return err
	}
	for _, t := range tokens {
		if t.Name == wantName && !t.Revoked {
			return glClient.RevokeProjectToken(projectID, t.ID)
		}
	}
	return nil
}

// --- GitHub-specific setup --------------------------------------------------

// githubBootstrapClient builds a GitHub client from the bootstrap PAT —
// used only for --create's repo-creation step (Spec §7: "For --attach, no
// bootstrap PAT is needed").
func githubBootstrapClient() (*github.Client, error) {
	bootstrapPAT, err := podman.GetSecretValue("devsys-bootstrap-github-token")
	if err != nil {
		return nil, fmt.Errorf("cannot read bootstrap GitHub PAT (run 'devsys auth github' first): %w", err)
	}
	return github.NewClient(bootstrapPAT), nil
}

func setupGitHubRemote(reader *bufio.Reader, projectName string, st *repoAuthStatus, mode string) (remoteURL, tokenValue string, err error) {
	var ownerRepo string
	if mode == "create" {
		bootstrapClient, err := githubBootstrapClient()
		if err != nil {
			return "", "", err
		}

		defaultName := repoDefaultName(projectName, st.Repo.RelPath)
		fmt.Printf("  Repo name [%s]: ", defaultName)
		line, _ := reader.ReadString('\n')
		name := strings.TrimSpace(line)
		if name == "" {
			name = defaultName
		}
		_, fullName, err := bootstrapClient.CreateRepo(name)
		if err != nil {
			return "", "", fmt.Errorf("cannot create GitHub repo: %w", err)
		}
		fmt.Printf("  -> created https://github.com/%s\n", fullName)
		ownerRepo = fullName
	} else {
		fmt.Print("  owner/repo: ")
		line, _ := reader.ReadString('\n')
		ownerRepo = strings.TrimSpace(line)
		if ownerRepo == "" {
			return "", "", fmt.Errorf("owner/repo cannot be empty")
		}
	}

	tokenValue, err = promptAndVerifyGitHubPAT(reader, ownerRepo)
	if err != nil {
		return "", "", err
	}
	remoteURL, err = embedTokenInHTTPSURL("https://github.com/"+ownerRepo, "x-access-token", tokenValue)
	if err != nil {
		return "", "", fmt.Errorf("cannot build remote URL: %w", err)
	}
	fmt.Println("  -> attached, remote wired")
	return remoteURL, tokenValue, nil
}

// setupGitHubRemoteScriptable is setupGitHubRemote's non-interactive
// counterpart: nameOrTarget comes from --name/--attach and the PAT from
// --token, never prompted (Spec §9: the scriptable form never prompts).
func setupGitHubRemoteScriptable(projectName string, st *repoAuthStatus, mode, nameOrTarget, token string) (remoteURL, tokenValue string, err error) {
	var ownerRepo string
	if mode == "create" {
		bootstrapClient, err := githubBootstrapClient()
		if err != nil {
			return "", "", err
		}
		name := nameOrTarget
		if name == "" {
			name = repoDefaultName(projectName, st.Repo.RelPath)
		}
		_, fullName, err := bootstrapClient.CreateRepo(name)
		if err != nil {
			return "", "", fmt.Errorf("cannot create GitHub repo: %w", err)
		}
		ownerRepo = fullName
	} else {
		ownerRepo = nameOrTarget
		if ownerRepo == "" {
			return "", "", fmt.Errorf("--attach requires a target (owner/repo)")
		}
	}

	if token == "" {
		return "", "", fmt.Errorf("--token is required for GitHub in non-interactive mode")
	}
	if err := verifyGitHubPAT(ownerRepo, token); err != nil {
		return "", "", fmt.Errorf("cannot verify %s with this PAT: %w", ownerRepo, err)
	}
	remoteURL, err = embedTokenInHTTPSURL("https://github.com/"+ownerRepo, "x-access-token", token)
	if err != nil {
		return "", "", fmt.Errorf("cannot build remote URL: %w", err)
	}
	return remoteURL, token, nil
}

// --- Shared helpers ---------------------------------------------------------

// embedTokenInHTTPSURL builds an HTTPS remote URL with the bearer token
// embedded as userinfo, e.g. https://oauth2:<TOKEN>@gitlab.com/owner/repo.git
// — the mechanism both platforms now use identically (Git Remote &
// Credential Spec §7's corrected GitHub decision).
func embedTokenInHTTPSURL(rawURL, user, token string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("cannot parse %q: %w", rawURL, err)
	}
	u.User = url.UserPassword(user, token)
	if !strings.HasSuffix(u.Path, ".git") {
		u.Path += ".git"
	}
	return u.String(), nil
}

// repoDefaultName suggests a platform project/repo name for "create" mode:
// the project name itself for the workspace-root repo, or
// "<project>-<relpath>" for any other repo in a multi-repo project.
func repoDefaultName(projectName, relPath string) string {
	if relPath == "." {
		return projectName
	}
	slug := strings.ReplaceAll(strings.ReplaceAll(relPath, "/", "-"), "\\", "-")
	return projectName + "-" + slug
}

func storeRepoSecret(secretName, value string, labels map[string]string) error {
	if podman.SecretExists(secretName) {
		if err := podman.DeleteSecret(secretName); err != nil {
			return fmt.Errorf("cannot remove existing secret %s: %w", secretName, err)
		}
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels["devsys"] = "true"
	return podman.CreateSecretFromStdin(secretName, value, labels)
}

func writeOriginRemote(repoPath, remoteURL string) error {
	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("cannot open repo at %s: %w", repoPath, err)
	}
	cfg, err := repo.Config()
	if err != nil {
		return fmt.Errorf("cannot read repo config: %w", err)
	}
	if cfg.Remotes == nil {
		cfg.Remotes = map[string]*gitconfig.RemoteConfig{}
	}
	cfg.Remotes["origin"] = &gitconfig.RemoteConfig{Name: "origin", URLs: []string{remoteURL}}
	return repo.SetConfig(cfg)
}

func clearOriginRemote(repoPath string) error {
	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("cannot open repo at %s: %w", repoPath, err)
	}
	cfg, err := repo.Config()
	if err != nil {
		return fmt.Errorf("cannot read repo config: %w", err)
	}
	delete(cfg.Remotes, "origin")
	return repo.SetConfig(cfg)
}

// --- Container recreation ----------------------------------------------------

// recreateContainerForAuth removes and recreates a project's container so
// its newly minted/changed per-repo secrets get mounted — Podman secret
// attachment is creation-time-only (Git Remote & Credential Spec §7).
// Recreation happens once per auth session (the caller only calls this
// after the whole interactive loop finishes), and only asks for
// confirmation if the container is actually running (Spec §9).
func recreateContainerForAuth(projectName string, interactive bool) error {
	containerName := fmt.Sprintf("devsys-%s", projectName)

	if podman.ContainerIsRunning(containerName) {
		fmt.Printf("\nThis will recreate %s's container to mount new/changed credentials.\n", containerName)
		fmt.Printf("%s is currently running (someone is inside it via 'devsys enter') — continuing\nwill end that session.\n", projectName)
		if interactive {
			reader := bufio.NewReader(os.Stdin)
			if !confirm(reader, "Proceed?") {
				fmt.Printf("  Skipped — credentials stored but not yet mounted. Run 'devsys auth %s' again, or 'devsys enter %s', to pick them up.\n", projectName, projectName)
				return nil
			}
		} else {
			// Scriptable form is meant to be non-interactive (Spec §9) — no
			// TTY to confirm against, so proceed rather than block forever
			// on a stdin read that will never come.
			fmt.Println("Proceeding without confirmation (non-interactive).")
		}
		if _, err := podman.RunPodman("stop", containerName); err != nil {
			return fmt.Errorf("cannot stop container: %w", err)
		}
	}

	projectPath, err := getProjectPath(containerName)
	if err != nil {
		return fmt.Errorf("cannot determine project path: %w", err)
	}
	imageTag := fmt.Sprintf("devsys-%s", projectName)

	if podman.ContainerExists(containerName) {
		if _, err := podman.RunPodman("rm", containerName); err != nil {
			return fmt.Errorf("cannot remove container: %w", err)
		}
	}
	if err := createProjectContainerWithRepoSecrets(containerName, projectName, projectPath, imageTag); err != nil {
		return fmt.Errorf("cannot recreate container: %w", err)
	}
	fmt.Printf("  Container %s recreated.\n", containerName)
	return nil
}

// createProjectContainerWithRepoSecrets creates the persistent container
// mounting every per-repo secret this project actually has as a plain file
// (Podman's default mount, /run/secrets/<secret-name>) — not the single
// type=env,target=GITLAB_TOKEN mount. Fixes the env-var collision a project
// with more than one per-repo token would otherwise hit (Git Remote &
// Credential Spec §7's multi-token fix). Used by this file's own
// recreateContainerForAuth, plus cmd/init.go and cmd/rebuild.go.
func createProjectContainerWithRepoSecrets(containerName, projectName, projectPath, imageTag string) error {
	trivyVolume := fmt.Sprintf("devsys-%s-trivy-db", projectName)

	for _, vol := range []string{"devsys-claude-auth", "devsys-codex-auth", trivyVolume} {
		if !podman.VolumeExists(vol) {
			if _, err := podman.RunPodman("volume", "create", "--label", "devsys=true", vol); err != nil {
				return fmt.Errorf("cannot create volume %s: %w", vol, err)
			}
		}
	}

	args := []string{
		"create",
		"--name", containerName,
		"--label", "devsys=true",
		"--volume", projectPath + ":" + defaultWorkspaceDest + ":Z",
		"--volume", "devsys-claude-auth:/root/.claude",
		"--volume", "devsys-codex-auth:/root/.codex",
		"--volume", trivyVolume + ":/root/.cache/trivy",
		"--env", "CLAUDE_CONFIG_DIR=/root/.claude",
		"--env", "CODEX_HOME=/root/.codex",
		"--env", "TRIVY_CACHE_DIR=/root/.cache/trivy",
		"--env", "IS_SANDBOX=1",
	}

	secretNames, err := projectRepoSecretNames(projectName, projectPath)
	if err != nil {
		return fmt.Errorf("cannot list project secrets: %w", err)
	}
	for _, name := range secretNames {
		args = append(args, "--secret", name) // default: mounted as a file at /run/secrets/<name>
	}
	args = append(args, imageTag)

	_, err = podman.RunPodman(args...)
	return err
}

// projectRepoSecretNames derives the exact per-repo credential secret names
// belonging to the repos currently discovered in workspaceRoot. Ownership is
// never inferred from a string prefix: project names are not delimiter-safe
// ("foo" is a prefix of "foo-bar"), so prefix matching could mount another
// project's credential into this container.
//
// The repo's own origin URL is the deterministic bridge from workspace path
// to secret name (Git Remote & Credential Spec §7/R15). Repos without an
// origin or without a stored secret are skipped. Duplicate names are emitted
// once when two local repos point at the same platform repo.
func projectRepoSecretNames(projectName, workspaceRoot string) ([]string, error) {
	repos, err := workspace.DiscoverRepos(workspaceRoot)
	if err != nil {
		return nil, err
	}

	var matched []string
	seen := make(map[string]bool)
	for _, repo := range repos {
		remoteURL, err := repo.OriginURL()
		if errors.Is(err, workspace.ErrNoOrigin) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read remote for %s: %w", repo.RelPath, err)
		}
		platform, err := workspace.PlatformFromURL(remoteURL)
		if err != nil {
			return nil, fmt.Errorf("cannot determine platform for %s: %w", repo.RelPath, err)
		}
		repoID, err := workspace.RepoIDFromURL(remoteURL)
		if err != nil {
			return nil, fmt.Errorf("cannot determine repo id for %s: %w", repo.RelPath, err)
		}
		name := workspace.SecretName(projectName, repoID, platform)
		if seen[name] || !podman.SecretExists(name) {
			continue
		}
		seen[name] = true
		matched = append(matched, name)
	}
	return matched, nil
}
