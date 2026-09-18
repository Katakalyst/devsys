package cmd

// devsysBaseImage is the fixed registry LOCATION (host + repo path) the
// shared meta-tooling image is published to — never a version, never a
// floating tag. It is consulted in exactly two bootstrap moments where no
// project-level record exists yet to derive a location from: `devsys setup`
// (no project involved at all) and a brand-new project's first `devsys
// init` (nothing in .devsys/Containerfile yet). Every other command derives
// the location from a project's own existing Containerfile instead (devsys
// CLI Spec, Section 12.3).
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
