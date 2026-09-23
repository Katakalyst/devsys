package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/katakalyst/devsys/internal/podman"
	"github.com/katakalyst/devsys/internal/workspace"
)

// --- Interactive listing and menu functions ---------------------------------

// gatherRepoStatuses runs the recursive discovery walk and derives each
// (repo, remote) pair's current credential state — purely by reading git
// config and checking which Podman secrets already exist, never from a
// stored config file (R15). A repo with no remotes yet yields one "no
// remote" row; a repo with one or more remotes yields one row per remote
// (Git Remote & Credential Spec §7's multi-remote decision) — every remote
// is enumerated, not just "origin".
func gatherRepoStatuses(projectName, workspaceRoot string) ([]repoAuthStatus, error) {
	repos, err := workspace.DiscoverRepos(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("cannot scan workspace for repos: %w", err)
	}

	statuses := make([]repoAuthStatus, 0, len(repos))
	for _, r := range repos {
		remotes, err := r.Remotes()
		if err != nil {
			return nil, fmt.Errorf("cannot read remotes for %s: %w", r.RelPath, err)
		}
		if len(remotes) == 0 {
			statuses = append(statuses, repoAuthStatus{Repo: r})
			continue
		}

		for _, rem := range remotes {
			st := repoAuthStatus{Repo: r, RemoteName: rem.Name, HasRemote: true, RemoteURL: rem.URL}

			platform, err := workspace.PlatformFromURL(rem.URL)
			if err != nil {
				return nil, fmt.Errorf("cannot determine platform for %s (%s): %w", r.RelPath, rem.Name, err)
			}
			st.Platform = platform

			repoID, err := workspace.RepoIDFromURL(rem.URL)
			if err != nil {
				return nil, fmt.Errorf("cannot determine repo id for %s (%s): %w", r.RelPath, rem.Name, err)
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
	}
	return statuses, nil
}

// printRepoListing shows the remote name alongside the repo path only once a
// repo actually has more than one remote — a single-remote repo's row stays
// exactly the pre-multi-remote format (Git Remote & Credential Spec §9: "a
// repo's remotes are otherwise shown collapsed onto its single row").
func printRepoListing(statuses []repoAuthStatus) {
	remoteCount := make(map[string]int)
	for _, st := range statuses {
		remoteCount[st.Repo.RelPath]++
	}
	for i, st := range statuses {
		label := st.Repo.RelPath
		if remoteCount[st.Repo.RelPath] > 1 && st.RemoteName != "" {
			label = fmt.Sprintf("%s (%s)", st.Repo.RelPath, st.RemoteName)
		}
		switch {
		case !st.HasRemote:
			fmt.Printf("  %d. %-15s no remote\n", i+1, label)
		case !st.HasToken:
			fmt.Printf("  %d. %-15s %-7s no token\n", i+1, label, st.Platform)
		default:
			fmt.Printf("  %d. %-15s %-7s %s\n", i+1, label, st.Platform, tokenExpiryText(st.ExpiresAt))
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
		return configureNoRemote(reader, projectName, st, "origin")
	case !st.HasToken:
		return configureRemoteNoToken(reader, projectName, st)
	default:
		return configureAlreadySet(reader, projectName, st)
	}
}

// configureNoRemote runs the create-or-attach wizard for a repo with no
// remote yet — the only branch that ever writes a fresh remote (Git Remote &
// Credential Spec §7: "auth never rewrites a remote it didn't create").
// remoteName is "origin" for a repo's first-ever remote (the normal case,
// via configureRepo above); changeRepoTarget below reuses this same wizard
// with the existing remote's own name preserved, for a deliberate re-target.
func configureNoRemote(reader *bufio.Reader, projectName string, st *repoAuthStatus, remoteName string) (bool, error) {
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
		remoteURL, tokenValue, labels, err = setupGitHubRemote(reader, projectName, st, mode)
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
	secretName := workspace.SecretName(projectName, repoID, platform)
	if err := storeRepoSecret(secretName, tokenValue, labels); err != nil {
		return false, err
	}

	fmt.Printf("  -> remote wired, token stored (%s)\n", secretName)
	st.RemoteName = remoteName
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
		tokenValue, labels, err = promptAndVerifyGitHubPAT(reader, repoPath)
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
	remoteName := st.RemoteName
	if err := revokeAndDeleteSecret(st); err != nil {
		return false, err
	}
	if err := clearRemote(st.Repo.AbsPath, remoteName); err != nil {
		return false, err
	}
	st.HasRemote = false
	st.HasToken = false
	st.RemoteURL = ""
	st.Platform = ""
	st.SecretName = ""
	st.ExpiresAt = ""
	return configureNoRemote(reader, projectName, st, remoteName)
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
		repoPath, err := workspace.PathFromURL(st.RemoteURL)
		if err != nil {
			return false, fmt.Errorf("cannot determine owner/repo: %w", err)
		}
		fmt.Println("  GitHub fine-grained PATs can't be rotated via API — create a new one, then paste it below.")
		fmt.Printf("  Once the new one is confirmed working, revoke the old PAT for %s yourself: %s\n", repoPath, githubTokensPageURL())
		tokenValue, labels, err = promptAndVerifyGitHubPAT(reader, repoPath)
		if err != nil {
			return false, err
		}
		userInfo = "x-access-token"
		if err := storeRepoSecret(st.SecretName, tokenValue, labels); err != nil {
			return false, err
		}
		st.ExpiresAt = labels["devsys.expires-at"]
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
		} else if err := writeRemote(st.Repo.AbsPath, st.RemoteName, newRemoteURL); err != nil {
			fmt.Fprintf(os.Stderr, "  Warning: could not update the remote with the new token: %v\n", err)
		} else {
			st.RemoteURL = newRemoteURL
			fmt.Println("  -> remote URL updated with the new token")
		}
	}
	return true, nil
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

// --- Prompts ----------------------------------------------------------------

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
