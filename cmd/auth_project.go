package cmd

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/workspace"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Scriptable-form flags (Phase 5, Git Remote & Credential Spec §9). Declared
// here alongside the code that reads them; registered on authCmd in
// cmd/auth.go's init(), same split already used for authGitLabURL above.
var (
	authScriptCreate    bool
	authScriptAttach    string
	authScriptRotate    bool
	authScriptRemove    bool
	authScriptName      string
	authScriptToken     string
	authScriptExpiresAt string
	authScriptForce     bool
)

// tokenExpiryWarnDays is the same 30-day threshold devsys enter's
// checkTokenExpiry already uses — one shared threshold for the whole CLI,
// not a second number to keep in sync (Git Remote & Credential Spec §9).
const tokenExpiryWarnDays = 30

// repoAuthStatus is one discovered repo's current credential state, as shown
// in `devsys auth <project>`'s interactive listing (Git Remote & Credential
// Spec §8/§9).
type repoAuthStatus struct {
	Repo workspace.Repo
	// RemoteName is "" until a remote exists. "origin" is the convention
	// name devsys itself gives a repo's first remote; any other name means
	// the agent set it (or a later remote) itself via `git remote add`
	// (Git Remote & Credential Spec §7's multi-remote decision).
	RemoteName string
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
	repoArg, remoteArg, platformArg, err := parseRepoAndPlatformArgs(extra)
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

	st, err := selectRepoForScriptable(statuses, repoArg, remoteArg)
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
// positional args into repo, remote, and platform: an arg equal to "gitlab"
// or "github" is the platform (order-independent, pulled out first);
// whatever's left is up to two positionals, repo then remote, shallow to
// deep (Spec §9's "<project> [repo] [remote] [platform]" — [remote] follows
// the same optionality rule as [repo], one level down: required only once
// the selected repo has more than one remote). A repo or remote whose name
// happens to literally be "gitlab"/"github" can't be addressed this way —
// an accepted, narrow edge case, same category as the
// project-name-vs-bootstrap-subcommand-name ambiguity already accepted
// elsewhere on this command.
func parseRepoAndPlatformArgs(extra []string) (repo, remote, platform string, err error) {
	if len(extra) > 3 {
		return "", "", "", fmt.Errorf("too many arguments — expected [repo] [remote] [platform]")
	}
	var positional []string
	for _, a := range extra {
		lower := strings.ToLower(a)
		if lower == "gitlab" || lower == "github" {
			if platform != "" {
				return "", "", "", fmt.Errorf("platform given twice")
			}
			platform = lower
			continue
		}
		positional = append(positional, a)
	}
	if len(positional) > 2 {
		return "", "", "", fmt.Errorf("unexpected extra argument %q", positional[2])
	}
	if len(positional) >= 1 {
		repo = positional[0]
	}
	if len(positional) >= 2 {
		remote = positional[1]
	}
	return repo, remote, platform, nil
}

// selectRepoForScriptable resolves which (repo, remote) row a scriptable
// invocation applies to. Repo: the explicitly named one, or the project's
// only one if none was given — Spec §9's stated error case: "[repo] omitted
// with 2+ repos present -> error listing the actual discovered paths, not a
// guess." Remote: the same rule one level down — explicitly named, or the
// repo's only remote row if none was given; omitted with 2+ remote rows on
// the selected repo errors listing the actual discovered remote names.
func selectRepoForScriptable(statuses []repoAuthStatus, repoArg, remoteArg string) (*repoAuthStatus, error) {
	var candidates []*repoAuthStatus
	if repoArg != "" {
		for i := range statuses {
			if statuses[i].Repo.RelPath == repoArg {
				candidates = append(candidates, &statuses[i])
			}
		}
		if len(candidates) == 0 {
			return nil, fmt.Errorf("no repo at %q — discovered repos: %s", repoArg, joinRepoPaths(statuses))
		}
	} else {
		paths := make(map[string]bool)
		for i := range statuses {
			paths[statuses[i].Repo.RelPath] = true
		}
		if len(paths) != 1 {
			return nil, fmt.Errorf("project has more than one repo — specify which one: %s", joinRepoPaths(statuses))
		}
		for i := range statuses {
			candidates = append(candidates, &statuses[i])
		}
	}

	if remoteArg != "" {
		for _, c := range candidates {
			if c.RemoteName == remoteArg {
				return c, nil
			}
		}
		return nil, fmt.Errorf("no remote %q on repo %q — discovered remotes: %s", remoteArg, candidates[0].Repo.RelPath, joinRemoteNames(candidates))
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return nil, fmt.Errorf("repo %q has more than one remote — specify which one: %s", candidates[0].Repo.RelPath, joinRemoteNames(candidates))
}

func joinRepoPaths(statuses []repoAuthStatus) string {
	paths := make([]string, len(statuses))
	for i, st := range statuses {
		paths[i] = st.Repo.RelPath
	}
	return strings.Join(paths, ", ")
}

func joinRemoteNames(candidates []*repoAuthStatus) string {
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c.RemoteName != "" {
			names = append(names, c.RemoteName)
		}
	}
	return strings.Join(names, ", ")
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
	// Preserved across a --force replace so the existing remote's own name
	// (e.g. "upstream") survives being re-targeted, rather than being
	// renamed to "origin" — a force-replace changes what a remote points
	// at, not what it's called. A brand-new remote (st.RemoteName == "")
	// always gets devsys's first-remote convention name, "origin".
	remoteName := st.RemoteName
	if remoteName == "" {
		remoteName = "origin"
	}

	if st.HasRemote {
		if !authScriptForce {
			return false, fmt.Errorf("%q is already configured — use --force to replace, --remove to clear, or --rotate to refresh", st.Repo.RelPath)
		}
		if err := revokeAndDeleteSecret(st); err != nil {
			return false, err
		}
		if err := clearRemote(st.Repo.AbsPath, remoteName); err != nil {
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
		remoteURL, tokenValue, labels, err = setupGitHubRemoteScriptable(projectName, st, mode, nameOrTarget, authScriptToken, authScriptExpiresAt)
	default:
		return false, fmt.Errorf("unknown platform %q — must be gitlab or github", platformArg)
	}
	if err != nil {
		return false, err
	}

	if err := writeRemote(st.Repo.AbsPath, remoteName, remoteURL); err != nil {
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

	st.RemoteName = remoteName
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
		labels, err = githubExpiresAtLabels(authScriptExpiresAt)
		if err != nil {
			return false, err
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
		} else if err := writeRemote(st.Repo.AbsPath, st.RemoteName, newRemoteURL); err != nil {
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
// --attach/--rotate/--remove given): R11 "ensure" — confirm a credential is
// provisioned in devsys's local state, safe to call whether or not one already
// exists, never implying a discard of one that's still fine. This deliberately
// does not make a platform API health check: known expiry is warned from local
// metadata, while invalid/revoked/scope failures surface on actual git/glab/gh
// use. Spec §9: "devsys auth <project> [repo] # bare — only valid if repo
// already has a remote."
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
		labels, err = githubExpiresAtLabels(authScriptExpiresAt)
		if err != nil {
			return false, err
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

// githubExpiresAtLabels is promptGitHubExpiresAt's non-interactive
// counterpart: validates --expires-at instead of prompting (the scriptable
// form never prompts). Blank is accepted, same meaning as a blank prompt
// answer — "No expiration" or unknown, not an error.
func githubExpiresAtLabels(expiresAt string) (map[string]string, error) {
	if expiresAt == "" {
		return nil, nil
	}
	if _, err := time.Parse("2006-01-02", expiresAt); err != nil {
		return nil, fmt.Errorf("--expires-at must be YYYY-MM-DD, got %q", expiresAt)
	}
	return map[string]string{"devsys.expires-at": expiresAt}, nil
}

func revokeAndDeleteSecret(st *repoAuthStatus) error {
	switch st.Platform {
	case "gitlab":
		if err := revokeGitLabRepoToken(st.RemoteURL); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not revoke GitLab token via API: %v\n", err)
		}
	case "github":
		fmt.Println(githubManualRevokeReminder(st.RemoteURL))
	}
	if st.SecretName != "" && podman.SecretExists(st.SecretName) {
		if err := podman.DeleteSecret(st.SecretName); err != nil {
			return fmt.Errorf("cannot delete secret %s: %w", st.SecretName, err)
		}
	}
	return nil
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

// writeRemote writes (creating or overwriting) the named remote — generalized
// from the original origin-only writeOriginRemote to support any remote name
// (Git Remote & Credential Spec §7's multi-remote decision). Every call site
// still only ever writes a remote it's itself creating or explicitly
// re-targeting (via the [c] Change/--force path) — never rewriting a bare
// remote the agent set itself, per the spec's "auth never rewrites a remote
// it didn't create" rule.
func writeRemote(repoPath, remoteName, remoteURL string) error {
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
	cfg.Remotes[remoteName] = &gitconfig.RemoteConfig{Name: remoteName, URLs: []string{remoteURL}}
	return repo.SetConfig(cfg)
}

// clearRemote removes the named remote — generalized from clearOriginRemote
// the same way writeRemote is (see above).
func clearRemote(repoPath, remoteName string) error {
	repo, err := gogit.PlainOpen(repoPath)
	if err != nil {
		return fmt.Errorf("cannot open repo at %s: %w", repoPath, err)
	}
	cfg, err := repo.Config()
	if err != nil {
		return fmt.Errorf("cannot read repo config: %w", err)
	}
	delete(cfg.Remotes, remoteName)
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
				fmt.Printf("  Skipped — credentials are stored, but the running container still has its old secret mounts. Exit every active 'devsys enter %s' session, then run 'devsys rebuild %s'.\n", projectName, projectName)
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
//
// Claude/Codex volumes are per-project (devsys-<project>-claude-auth/
// -codex-auth). Each agent authenticates itself inside the container on first
// run; the volume persists chats, settings, and credentials across restarts.
// On first creation (volume didn't exist yet), settings.json is copied from
// the host if present — a one-time convenience seed that never overwrites
// existing content and never touches credentials.
func createProjectContainerWithRepoSecrets(containerName, projectName, projectPath, imageTag string) error {
	claudeVolume := agentAuthVolumeName(projectName, "claude")
	codexVolume := agentAuthVolumeName(projectName, "codex")
	trivyVolume := fmt.Sprintf("devsys-%s-trivy-db", projectName)

	for _, vol := range []string{claudeVolume, codexVolume, trivyVolume} {
		if !podman.VolumeExists(vol) {
			if _, err := podman.RunPodman("volume", "create", "--label", "devsys=true", vol); err != nil {
				return fmt.Errorf("cannot create volume %s: %w", vol, err)
			}
		}
	}

	// Seed settings (non-destructively) from host on first creation.
	home, err := os.UserHomeDir()
	if err == nil {
		seedAgentSettings(claudeVolume, filepath.Join(home, ".claude", "settings.json"), imageTag)
		seedAgentSettings(codexVolume, filepath.Join(home, ".codex", "settings.json"), imageTag)
	}

	args := []string{
		"create",
		"--name", containerName,
		"--label", "devsys=true",
		"--volume", projectPath + ":" + defaultWorkspaceDest + ":Z",
		"--volume", claudeVolume + ":/root/.claude",
		"--volume", codexVolume + ":/root/.codex",
		"--volume", trivyVolume + ":/root/.cache/trivy",
		"--env", "CLAUDE_CONFIG_DIR=/root/.claude",
		"--env", "CODEX_HOME=/root/.codex",
		"--env", "TRIVY_CACHE_DIR=/root/.cache/trivy",
		"--env", "IS_SANDBOX=1",
	}

	portMappings, err := readPortsFile(projectPath)
	if err != nil {
		return fmt.Errorf("cannot read ports file: %w", err)
	}
	for _, p := range portMappings {
		args = append(args, "-p", p)
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

// seedAgentSettings copies settings.json from the host into a named volume,
// but only when the volume is empty — once on first creation, never again.
// Preserves any data the agent has written (chats, credentials) on all
// subsequent calls. Silently skips when the host file is absent, the volume
// already has content, or any podman call fails.
func seedAgentSettings(volumeName, hostSettingsFile, imageTag string) {
	if _, err := os.Stat(hostSettingsFile); err != nil {
		return // nothing to seed
	}
	// Only seed an empty volume.
	out, err := podman.RunPodman(
		"run", "--rm",
		"--volume", volumeName+":/data:ro",
		"--entrypoint", "sh",
		imageTag,
		"-c", "ls -A /data 2>/dev/null",
	)
	if err != nil || strings.TrimSpace(out) != "" {
		return
	}
	_ = podman.RunPodmanLive(
		"run", "--rm",
		"--volume", hostSettingsFile+":/src/settings.json:ro",
		"--volume", volumeName+":/data",
		"--entrypoint", "sh",
		imageTag,
		"-c", "cp /src/settings.json /data/settings.json",
	)
}

// projectRepoSecretNames derives the exact per-repo credential secret names
// belonging to the repos currently discovered in workspaceRoot — one per
// configured remote, not just "origin" (Git Remote & Credential Spec §7's
// multi-remote decision). Ownership is never inferred from a string prefix:
// project names are not delimiter-safe ("foo" is a prefix of "foo-bar"), so
// prefix matching could mount another project's credential into this
// container.
//
// Each remote's own URL is the deterministic bridge from workspace path to
// secret name (Spec §7/R15). A repo with no remotes contributes nothing.
// Duplicate names are emitted once when two remotes (on the same repo, or
// across different repos) resolve to the same secret — e.g. the same
// platform repo pointed at by more than one local remote.
func projectRepoSecretNames(projectName, workspaceRoot string) ([]string, error) {
	repos, err := workspace.DiscoverRepos(workspaceRoot)
	if err != nil {
		return nil, err
	}

	var matched []string
	seen := make(map[string]bool)
	for _, repo := range repos {
		remotes, err := repo.Remotes()
		if err != nil {
			return nil, fmt.Errorf("cannot read remotes for %s: %w", repo.RelPath, err)
		}
		for _, rem := range remotes {
			platform, err := workspace.PlatformFromURL(rem.URL)
			if err != nil {
				return nil, fmt.Errorf("cannot determine platform for %s (%s): %w", repo.RelPath, rem.Name, err)
			}
			repoID, err := workspace.RepoIDFromURL(rem.URL)
			if err != nil {
				return nil, fmt.Errorf("cannot determine repo id for %s (%s): %w", repo.RelPath, rem.Name, err)
			}
			name := workspace.SecretName(projectName, repoID, platform)
			if seen[name] || !podman.SecretExists(name) {
				continue
			}
			seen[name] = true
			matched = append(matched, name)
		}
	}
	return matched, nil
}
