//go:build !dev

package gitlab

// defaultDryRun is false in production builds.
var defaultDryRun = false
