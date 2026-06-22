// Package clibuild is the shared command framework + CORE command set that both
// the standalone cockroach-cli binary and the reporters product CLI are built
// from. It owns the stdlib `flag`-based command registry, the dispatch loop +
// exit-code contract, the env/DSN/registry helpers the core commands need, and
// the cross-product CORE commands themselves (version, pack, pack-upgrade,
// lang). Product CLIs append their own commands and dispatch through the same
// loop, so the glue lives in ONE place.
//
// Stdlib `flag` only — no cobra. House style; cobra is a later stage.
package clibuild

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"

	"github.com/cockroachnetworkorg/cockroach-cli/pkg/pack"
)

// canonicalName is the framework CLI's formal, distribution-level name — the
// fallback when the invoked name can't be determined (e.g. a `go run` temp
// build with an odd argv[0]). The name operators actually TYPE is whatever the
// installed binary (or a symlinked alias) is named; ProgName() reflects that at
// runtime so help/errors/banners match how the tool was invoked.
const canonicalName = "cockroach-cli"

// DefaultDSN is the conventional local-dev DSN. The host port (:55432) is
// offset from Postgres' standard :5432 to dodge a developer's own local
// Postgres. DSN() falls back to this ONLY when $DATABASE_URL is unset AND
// APP_ENV is not production; in production the CLI refuses to default.
const DefaultDSN = "postgres://cockroach:cockroach@localhost:55432/cockroach_network?sslmode=disable"

// Command is one CLI subcommand. Exported so product CLIs can build their own
// registry entries and hand the combined set to Dispatch.
type Command struct {
	Name    string
	Summary string
	// Run receives the args AFTER the subcommand name. A non-nil error signals
	// failure (Dispatch prints it + returns a non-zero exit code).
	Run func(ctx context.Context, args []string) error
}

// ErrUsage marks an error as a usage problem (bad args, missing required flag,
// unknown subcommand). Dispatch unwraps and returns exit 2 instead of 1. The
// sentinel itself never appears in the printed message.
var ErrUsage = errors.New("usage error")

// UsageErrorf returns an error tagged as a usage problem (exit 2). The caller's
// message text is preserved verbatim; the sentinel is wrapped behind it so
// errors.Is(err, ErrUsage) is the only way to detect it.
func UsageErrorf(format string, a ...any) error {
	return &wrappedUsageError{inner: fmt.Errorf(format, a...)}
}

type wrappedUsageError struct{ inner error }

func (w *wrappedUsageError) Error() string { return w.inner.Error() }
func (w *wrappedUsageError) Unwrap() error { return ErrUsage }

// ProgName returns the name the binary was invoked as: basename(argv[0]) minus
// any .exe / .test suffix. This is the busybox/git convention — an operator who
// installs the binary under a different name (or symlinks a short alias) sees
// THAT name echoed back everywhere. Falls back to canonicalName when argv[0] is
// empty/path-ish (defensive).
func ProgName() string {
	base := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	// `go test` builds a binary named "<pkg>.test"; normalise it so help/usage
	// output (and the assertions that check it) show the canonical command.
	base = strings.TrimSuffix(base, ".test")
	if base == "" || base == "." || base == string(os.PathSeparator) {
		return canonicalName
	}
	return base
}

// LoadDotenvBestEffort loads the same env file the SERVER reads (backend/.env —
// godotenv never overrides variables already exported), so the CLI and the
// server agree on DATABASE_URL / APP_ENV / CN_MASTER_KEY right after `init`
// writes them. Probes ".env" (cwd = backend/) then "backend/.env" (cwd = repo
// root); the quiet stderr note keeps stdout clean for scripted callers.
func LoadDotenvBestEffort() {
	for _, f := range []string{".env", filepath.Join("backend", ".env")} {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		if err := godotenv.Load(f); err == nil {
			fmt.Fprintf(os.Stderr, "(env: loaded %s — exported variables take precedence)\n", f)
		}
		return
	}
}

// DSN returns the target database DSN, falling back to the dev cluster.
//
// Falls back to DefaultDSN ONLY when $APP_ENV does not look like a production
// environment. In production we never silently default to localhost:55432 —
// the operator must set $DATABASE_URL explicitly. (This keeps a misconfigured
// prod box from running migrations against a stranded loopback Postgres.)
func DSN() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	if IsProductionEnv() {
		return ""
	}
	return DefaultDSN
}

// IsProductionEnv reports whether the current process looks like it's running
// in a production deployment. Conservative — anything outside the known dev /
// staging values still defaults to "be safe" via DSN()'s refusal path.
func IsProductionEnv() bool {
	switch os.Getenv("APP_ENV") {
	case "production", "prod":
		return true
	}
	return false
}

// RegistryURL resolves the pack registry catalog URL: $COCKROACH_REGISTRY_URL
// when set, else pkg/pack's default.
func RegistryURL() string {
	if v := strings.TrimSpace(os.Getenv("COCKROACH_REGISTRY_URL")); v != "" {
		return v
	}
	return pack.DefaultRegistryURL
}
