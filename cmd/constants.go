package cmd

import "strings"

// imageHost returns the registry host portion of a "host/repo/path" image
// location (e.g. "ghcr.io" from devsysBaseImage).
func imageHost(location string) string {
	host, _, found := strings.Cut(location, "/")
	if !found {
		return ""
	}
	return host
}

// devsysBaseImage is the fixed registry LOCATION (host + repo path) the
// shared meta-tooling image is published to — never a version, never a
// floating tag. Consulted from Go in exactly one bootstrap moment where no
// project-level record exists yet to derive a location from: a brand-new
// project's first `devsys init` (nothing in .devsys/Containerfile yet).
// Every other command derives the location from a project's own existing
// Containerfile instead (devsys CLI Spec, Section 12.3). The installer
// scripts (install.sh/install.ps1) look up and pull the newest devsys-base
// version themselves, directly against GHCR — that lookup is independent of
// this constant, since it runs before devsys itself is even installed.
//
// Not user-configurable: there is no supported use case for pointing devsys
// at a different base image location, and letting it be swapped would
// silently undermine guarantees this architecture depends on elsewhere
// (e.g. the Claude Code >=2.1.61 version floor, Container Architecture Spec
// Section 5.3) that assume everyone is running images published from here.
//
// Hosted on GHCR under the devsys-base repo (github.com/katakalyst/devsys-base)
// — devsys CLI Spec, Section 12.6.
//
// A var, not a const, so tests can point it at a local httptest server
// instead of the real registry (same pattern as cmd/update.go's
// releasesAPIURL).
var devsysBaseImage = "ghcr.io/katakalyst/devsys-base"
