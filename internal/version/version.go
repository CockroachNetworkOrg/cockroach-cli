// Package version is the single source of truth for the standalone
// cockroach-cli build's identity. The values are injected at build time via
// -ldflags "-X" (see .goreleaser.yml); a plain `go build` or `go test` leaves
// the dev defaults.
//
// This is this module's OWN copy — deliberately independent of the reporters
// backend's internal/version. The two binaries version separately.
package version

// Injected at build time. Keep these as package-level vars (not consts) — the
// linker can only rewrite vars via -X.
var (
	// Version is the released semver tag, e.g. "v1.2.3". "dev" for an
	// un-tagged local build.
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "unknown"
	// BuildDate is the RFC3339 UTC build timestamp.
	BuildDate = "unknown"
)

// Short returns the bare version tag (e.g. "v1.2.3").
func Short() string { return Version }

// Full returns a human string: "v1.2.3 (abc1234, 2026-06-16T10:20:30Z)".
func Full() string {
	return Version + " (" + Commit + ", " + BuildDate + ")"
}
