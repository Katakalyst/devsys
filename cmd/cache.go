package cmd

import (
	"os"
	"path/filepath"
	"time"
)

// staleCheckInterval is how long a throttled check's result is trusted
// before devsys enter checks live again (devsys CLI Spec, Section 12.5).
const staleCheckInterval = 24 * time.Hour

// checkMarkerPath returns the path to the freely-deletable cache marker
// used to throttle a named staleness check. This is a cache, not a config:
// nothing it holds ever changes what gets built or which host is used, only
// how soon the next live check happens. Missing is fine — the next check
// just runs live instead of using a stale answer.
func checkMarkerPath(name string) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "devsys", name), nil
}

// checkThrottled reports whether the named check was already performed
// within staleCheckInterval, based purely on a marker file's mtime — no
// content to read or parse. Any error (no cache dir, marker missing) is
// treated as "not throttled, check live," never as a reason to fail.
func checkThrottled(name string) bool {
	path, err := checkMarkerPath(name)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < staleCheckInterval
}

// markChecked touches the named check's marker file, creating its parent
// directory if needed. Best-effort: a failure here just means the next
// `devsys enter` checks live again, not a real problem.
func markChecked(name string) {
	path, err := checkMarkerPath(name)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	f.Close()
}
