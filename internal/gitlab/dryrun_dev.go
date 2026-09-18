//go:build dev

package gitlab

// defaultDryRun is set to true in dev builds so every Client created via
// NewClient gets DryRun enabled automatically.
var defaultDryRun = true
