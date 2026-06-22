package clibuild

// `cockroach-cli pack …` — the country data-pack package manager. Mirrors the
// pattern that shadcn, brew, and `wp language core install` all converge on:
//
//   - Core ships EMPTY. No country data is baked into the binary.
//   - A small static `registry.json` (the catalog) indexes every available pack.
//   - Each pack is a checksummed, versioned `.csv.gz` published on the country's
//     `cockroach-<cc>-seed` repo's GitHub Releases.
//   - The CLI fetches, verifies the sha256, stages, COPYs, UPSERTs (idempotent),
//     and records the install in `installed_packs`. Same flow on dev/stage/prod.
//
// Operators almost never need this directly — the admin "Data & Languages" panel
// drives the same backend importer. This command exists for CI/automation and
// for the rare operator who'd rather drive from a terminal.
//
// Subcommands:
//   cockroach-cli pack list                        the registry catalogue (countries)
//   cockroach-cli pack list --langs                language packs known to the registry
//   cockroach-cli pack list --boundaries           recommended boundary GeoJSONs known to the registry
//   cockroach-cli pack list --all                  countries + language packs + boundaries
//   cockroach-cli pack list --installed            what THIS database has loaded
//   cockroach-cli pack info    --country PS        details for one country
//   cockroach-cli pack info    --lang hi           details for one language (Phase-2 metadata only)
//   cockroach-cli pack info    --boundary IN       details for one boundary GeoJSON (Phase-2)
//   cockroach-cli pack install --country PS        fetch + apply (idempotent)
//   cockroach-cli pack install --country PS --dry-run    plan, no writes
//   cockroach-cli pack install --lang hi           NOT YET AVAILABLE — redirects to `cockroach-cli lang import`
//   cockroach-cli pack install --boundary IN       fetch + sha256-verify + UPSERT into admin_units
//   cockroach-cli pack install --boundary IN --dry-run     fetch + verify + parse; no writes
//   cockroach-cli pack install --boundary IN --verify-only fetch + sha256-verify only; no parse, no writes
//   cockroach-cli pack verify  --country PS        re-hash without applying
//   cockroach-cli pack verify  --lang hi           Phase-2 metadata-only verify (no bundle download yet)
//   cockroach-cli pack verify  --boundary IN       HEAD + GET + sha256-verify the GeoJSON bytes
//   cockroach-cli pack search  <query>             substring search across countries
//
// Env:
//   COCKROACH_REGISTRY_URL    override the default catalog URL (for forks /
//                             air-gapped mirrors / testing)
//   DATABASE_URL              target Postgres DSN (per the top-level CLI)
//
// LANGUAGE-PACK PHASE-2 NOTE
//
// `--lang` is recognised everywhere the registry shape supports it (info /
// verify / install), but the actual download + admin-API push path is NOT yet
// wired through `pkg/pack`. Until the cockroach-world-language CI publishes
// per-language bundles + their sha256s into the central registry,
// `cockroach-cli pack install --lang <code>` exits non-zero (2) with a clear
// redirect to `cockroach-cli lang import --lang <code>` (the sibling-repo
// shell-out that already works today). See plan/pack-cli-and-registry.md
// follow-up #3.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cockroachnetworkorg/cockroach-cli/pkg/pack"
)

// CmdPack is the `pack` subcommand entry point. Exported so a product CLI can
// expose it directly (e.g. reporters' bootstrap/setup call it).
func CmdPack(ctx context.Context, args []string) error {
	if len(args) == 0 {
		packUsage()
		return UsageErrorf("missing subcommand (try: list | info | install | upgrade | verify | search)")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "-h", "--help", "help":
		packUsage()
		return nil
	case "list":
		return cmdPackList(ctx, rest)
	case "info":
		return cmdPackInfo(ctx, rest)
	case "install":
		return CmdPackInstall(ctx, rest)
	case "upgrade":
		return CmdPackUpgrade(ctx, rest)
	case "verify":
		return cmdPackVerify(ctx, rest)
	case "search":
		return cmdPackSearch(ctx, rest)
	default:
		packUsage()
		return UsageErrorf("unknown pack subcommand %q", sub)
	}
}

func packUsage() {
	fmt.Print(`Usage: cockroach-cli pack <subcommand> [flags]

Subcommands:
  list                      list country packs available in the registry
  list --langs              list language packs available in the registry
  list --boundaries         list recommended boundary GeoJSONs in the registry
  list --all                list country + language + boundary entries
  list --installed          list packs already loaded into $DATABASE_URL
  info    --country <cc>    show one country's packs (files, sizes, sha256)
  info    --lang <code>     show one language's bundles (Phase-2; metadata only)
  info    --boundary <cc>   show one boundary GeoJSON's metadata (Phase-2)
  install --country <cc>    download + verify + UPSERT into $DATABASE_URL
          [--version <v>]   pin a specific version (default: registry's latest)
          [--from-dir <p>]  load packs from a LOCAL dir (pre-release / pack-author
                            dev) instead of the registry's release_url; bytes are
                            still sha256/size-verified against the registry.
                            Env fallback: COCKROACH_PACK_DIR
          [--dry-run]       fetch + verify only; write nothing
  install --lang <code>     NOT YET AVAILABLE — prints redirect to
                            ` + "`cockroach-cli lang import --lang <code>`" + ` and exits 2
  install --boundary <cc>   download + sha256-verify + UPSERT into admin_units
          [--dry-run]       fetch + verify + parse only; no writes
          [--verify-only]   fetch + sha256-verify only; no parse, no writes
  upgrade                   list outdated installed packs and (with --apply) upgrade them
          [--country <cc>]  limit to one country
          [--dry-run]       fetch + verify only; no writes
          [--apply | -y]    actually run the upgrade (without this flag = plan only)
  verify  --country <cc>    re-hash the registry's packs WITHOUT applying
  verify  --lang <code>     Phase-2 metadata-only verify (no bundle download yet)
  verify  --boundary <cc>   HEAD + GET + sha256-verify the registry's GeoJSON URL
  search  <query>           substring-match country names + codes

Env:
  COCKROACH_REGISTRY_URL    override the default catalog URL
  DATABASE_URL              target Postgres DSN (same as the top-level CLI)

Examples:
  cockroach-cli pack list                              # registry catalog (countries)
  cockroach-cli pack list --installed                  # what's loaded in $DATABASE_URL
  cockroach-cli pack info    --country IN              # files, sizes, sha256 for IN
  cockroach-cli pack install --country IN              # fetch + verify + UPSERT
  cockroach-cli pack install --country IN --from-dir ../cockroach-india-seed/packs/IN  # local dev / pre-release
  cockroach-cli pack install --country IN --dry-run    # plan only, no writes
  cockroach-cli pack install --boundary IN --verify-only  # network + sha integrity check
  cockroach-cli pack upgrade --dry-run                 # what's out of date?
  cockroach-cli pack verify  --country IN              # re-hash registry packs (no DB write)
  cockroach-cli pack search  india                     # substring match across countries
`)
}

// ── list ──────────────────────────────────────────────────────────────────────

func cmdPackList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pack list", flag.ContinueOnError)
	installed := fs.Bool("installed", false, "list packs already installed in $DATABASE_URL (not the registry)")
	langs := fs.Bool("langs", false, "list language packs instead of country packs")
	boundaries := fs.Bool("boundaries", false, "list recommended boundary GeoJSONs instead of country packs")
	all := fs.Bool("all", false, "list country, language, and boundary entries")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *installed {
		return listInstalled(ctx)
	}
	url := RegistryURL()
	fmt.Printf("==> registry: %s\n", url)
	reg, err := pack.FetchRegistry(ctx, url)
	if err != nil {
		return err
	}

	// Default (no flags) = countries only, matching the prior behaviour.
	// Any of --langs / --boundaries narrows to just that view; --all shows all three.
	showCountries := (!*langs && !*boundaries) || *all
	showLangs := *langs || *all
	showBoundaries := *boundaries || *all

	if showCountries {
		if *all {
			fmt.Println("Countries:")
		}
		printCountryTable(reg)
	}
	if showLangs {
		if *all {
			fmt.Println()
			fmt.Println("Languages:")
		}
		printLangTable(reg)
	}
	if showBoundaries {
		if *all {
			fmt.Println()
			fmt.Println("Boundaries:")
		}
		printBoundaryTable(reg)
	}
	return nil
}

func printCountryTable(reg *pack.Registry) {
	if len(reg.Countries) == 0 {
		fmt.Println("(no countries published yet)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CODE\tCOUNTRY\tVERSION\tPACKS\tLICENSE")
	for _, cc := range reg.CountryCodes() {
		c, _ := reg.Country(cc)
		lic := c.License
		if lic == "" {
			lic = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", cc, c.Name, c.Version, len(c.Packs), lic)
	}
	tw.Flush()
}

func printLangTable(reg *pack.Registry) {
	if len(reg.Langs) == 0 {
		fmt.Println("(none — community translations still ship via `cockroach-cli lang import` for now; see plan/support.md / pack-cli-and-registry.md follow-up #3)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CODE\tLANGUAGE\tNATIVE\tVERSION\tBUNDLES\tLICENSE")
	for _, code := range reg.LangCodes() {
		l, _ := reg.Lang(code)
		native := l.NativeName
		if native == "" {
			native = "—"
		}
		lic := l.License
		if lic == "" {
			lic = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", code, l.Name, native, l.Version, len(l.Bundles), lic)
	}
	tw.Flush()
	fmt.Println("(Phase-2: `cockroach-cli pack install --lang` not yet wired; use `cockroach-cli lang import --lang <code>` for now.)")
}

func printBoundaryTable(reg *pack.Registry) {
	if len(reg.Boundaries) == 0 {
		fmt.Println("(none yet — see plan/bootstrap-and-jurisdiction.md)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CODE\tNAME\tVERSION\tSIZE\tLICENSE")
	for _, cc := range reg.BoundaryCodes() {
		b, _ := reg.Boundary(cc)
		lic := b.License
		if lic == "" {
			lic = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", cc, b.Name, b.Version, humanBytes(b.SizeBytes), lic)
	}
	tw.Flush()
	fmt.Println("(Install with `cockroach-cli pack install --boundary <cc>` — UPSERTs polygons into admin_units. See plan/bootstrap-and-jurisdiction.md.)")
}

func listInstalled(ctx context.Context) error {
	db, err := openPool(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	installed, err := pack.ListInstalled(ctx, db)
	if err != nil {
		return err
	}
	if len(installed) == 0 {
		fmt.Println("no packs installed (run: cockroach-cli pack install --country <cc>)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "COUNTRY\tFILE\tTABLE\tVERSION\tROWS\tINSTALLED")
	for _, p := range installed {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			p.CountryCode, p.PackFile, p.TargetTable, p.Version,
			p.RowsLoaded, p.InstalledAt.UTC().Format(time.RFC3339))
	}
	tw.Flush()
	return nil
}

// ── info ──────────────────────────────────────────────────────────────────────

func cmdPackInfo(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pack info", flag.ContinueOnError)
	country := fs.String("country", "", "ISO 3166-1 alpha-2 country code (e.g. PS)")
	lang := fs.String("lang", "", "ISO 639-1/3 language code (e.g. hi). Phase-2 — metadata only.")
	boundary := fs.String("boundary", "", "ISO 3166-1 alpha-2 country code for the registered boundary GeoJSON (e.g. IN). Phase-2 — metadata only.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !exactlyOne(*country != "", *lang != "", *boundary != "") {
		return UsageErrorf("provide exactly one of --country <cc>, --lang <code>, or --boundary <cc>")
	}
	reg, err := pack.FetchRegistry(ctx, RegistryURL())
	if err != nil {
		return err
	}
	if *lang != "" {
		return printLangInfo(reg, *lang)
	}
	if *boundary != "" {
		return printBoundaryInfo(reg, *boundary)
	}
	cc, err := normaliseCountry(*country)
	if err != nil {
		return err
	}
	entry, ok := reg.Country(cc)
	if !ok {
		return UsageErrorf("country %s not in the registry — see `cockroach-cli pack list`", cc)
	}
	fmt.Printf("%s — %s\n", cc, entry.Name)
	fmt.Printf("  version     : %s\n", entry.Version)
	fmt.Printf("  release_url : %s\n", entry.ReleaseURL)
	if entry.License != "" {
		fmt.Printf("  license     : %s\n", entry.License)
	}
	if entry.Sources != "" {
		fmt.Printf("  sources     : %s\n", entry.Sources)
	}
	fmt.Println("  packs:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    FILE\tTABLE\tROWS\tSIZE\tSHA256")
	for _, p := range entry.Packs {
		fmt.Fprintf(tw, "    %s\t%s\t%d\t%s\t%s…\n",
			p.File, p.Table, p.ExpectedRows, humanBytes(p.SizeBytes), short12(p.SHA256))
	}
	tw.Flush()
	return nil
}

// printLangInfo renders one language entry's metadata. The actual install path
// is not yet implemented — the footer makes that explicit and points operators
// at the sibling-repo shell-out that still works today.
func printLangInfo(reg *pack.Registry, code string) error {
	code = normaliseLang(code)
	l, ok := reg.Lang(code)
	if !ok {
		return UsageErrorf("language %q not in the registry — see `cockroach-cli pack list --langs` (and note: Phase-2 — community translations still ship via `cockroach-cli lang import` for now)", code)
	}
	fmt.Printf("%s — %s\n", code, l.Name)
	if l.NativeName != "" {
		fmt.Printf("  native      : %s\n", l.NativeName)
	}
	fmt.Printf("  version     : %s\n", l.Version)
	fmt.Printf("  release_url : %s\n", l.ReleaseURL)
	if l.License != "" {
		fmt.Printf("  license     : %s\n", l.License)
	}
	fmt.Println("  bundles:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "    APP\tFILE\tNAMESPACES\tKEYS\tSIZE\tSHA256")
	for _, b := range l.Bundles {
		fmt.Fprintf(tw, "    %s\t%s\t%d\t%d\t%s\t%s…\n",
			b.App, b.File, b.NamespaceCount, b.KeyCount, humanBytes(b.SizeBytes), short12(b.SHA256))
	}
	tw.Flush()
	fmt.Println()
	fmt.Println("Phase-2: language packs are not yet installable via `cockroach-cli pack install`.")
	fmt.Printf("To import this language into a running instance today, run:\n  cockroach-cli lang import --lang %s --instance https://your-instance.org --token \"$ADMIN_TOKEN\"\n", code)
	return nil
}

// printBoundaryInfo renders one boundary entry's metadata. The actual install
// path is Phase-2 — the footer is the operational workaround (set
// VITE_MAP_BOUNDARY_URL directly) until the wiring lands.
func printBoundaryInfo(reg *pack.Registry, code string) error {
	cc, err := normaliseCountry(code)
	if err != nil {
		return err
	}
	b, ok := reg.Boundary(cc)
	if !ok {
		return UsageErrorf("boundary %s not in the registry — see `cockroach-cli pack list --boundaries` (and note: Phase-2 — community boundary GeoJSONs are opt-in via VITE_MAP_BOUNDARY_URL; see plan/bootstrap-and-jurisdiction.md)", cc)
	}
	fmt.Printf("%s — %s\n", cc, b.Name)
	fmt.Printf("  version    : %s\n", b.Version)
	fmt.Printf("  url        : %s\n", b.URL)
	fmt.Printf("  size       : %s\n", humanBytes(b.SizeBytes))
	fmt.Printf("  sha256     : %s\n", b.SHA256)
	if b.License != "" {
		fmt.Printf("  license    : %s\n", b.License)
	}
	if b.Sources != "" {
		fmt.Printf("  sources    : %s\n", b.Sources)
	}
	if b.Notes != "" {
		fmt.Printf("  notes      : %s\n", b.Notes)
	}
	if len(b.Files) > 0 {
		fmt.Println("  files:")
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "    ROLE\tFILE\tSIZE\tSHA256")
		// Stable order — country, states, districts, then anything else
		// alphabetical. Matches the install walker.
		order := []string{"country", "states", "districts", "subdistricts"}
		seen := map[string]bool{}
		for _, key := range order {
			f, ok := b.Files[key]
			if !ok {
				continue
			}
			seen[key] = true
			role := f.Role
			if role == "" {
				role = key
			}
			fmt.Fprintf(tw, "    %s\t%s\t%s\t%s…\n", role, f.File, humanBytes(f.SizeBytes), short12(f.SHA256))
		}
		for key, f := range b.Files {
			if seen[key] {
				continue
			}
			role := f.Role
			if role == "" {
				role = key
			}
			fmt.Fprintf(tw, "    %s\t%s\t%s\t%s…\n", role, f.File, humanBytes(f.SizeBytes), short12(f.SHA256))
		}
		tw.Flush()
	}
	fmt.Println()
	fmt.Println("Install:")
	fmt.Printf("  cockroach-cli pack install --boundary %s            # download + verify + UPSERT into admin_units\n", cc)
	fmt.Printf("  cockroach-cli pack install --boundary %s --dry-run  # plan only, no writes\n", cc)
	fmt.Println()
	fmt.Println("Legacy frontend-only mode (sets the SPA env var without DB write):")
	fmt.Printf("  VITE_MAP_BOUNDARY_URL=%s\n", b.URL)
	fmt.Println("See plan/bootstrap-and-jurisdiction.md for the legal posture per jurisdiction.")
	return nil
}

// exactlyOne returns true iff exactly one of the given booleans is true. Lets
// the multi-target `pack info` / `pack verify` flag validation read clean.
func exactlyOne(bs ...bool) bool {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n == 1
}

// ── install ───────────────────────────────────────────────────────────────────

// CmdPackInstall is exported so a product CLI can drive a country install
// directly (reporters' bootstrap/setup call it with []string{"--country", cc}).
func CmdPackInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pack install", flag.ContinueOnError)
	country := fs.String("country", "", "ISO 3166-1 alpha-2 country code (e.g. PS)")
	lang := fs.String("lang", "", "ISO 639-1/3 language code (e.g. hi). Phase-2 — not yet installable.")
	boundary := fs.String("boundary", "", "ISO 3166-1 alpha-2 country code for a registered boundary GeoJSON (e.g. IN).")
	version := fs.String("version", "", "pin a specific version (default: registry's latest)")
	dryRun := fs.Bool("dry-run", false, "fetch + verify, do not write to the database")
	verifyOnly := fs.Bool("verify-only", false, "fetch + sha256-verify only; skip parse and write (only meaningful with --boundary)")
	fromDir := fs.String("from-dir", os.Getenv("COCKROACH_PACK_DIR"),
		"load packs from a LOCAL directory (e.g. ../cockroach-india-seed/packs/IN) instead of the registry's release_url; still sha256/size-verified against the registry. Env fallback: COCKROACH_PACK_DIR")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !exactlyOne(*country != "", *lang != "", *boundary != "") {
		return UsageErrorf("provide exactly one of --country <cc>, --lang <code>, or --boundary <cc>")
	}
	if *lang != "" {
		return installLangPhase2Stub(ctx, *lang)
	}
	if *boundary != "" {
		return installBoundary(ctx, *boundary, *dryRun, *verifyOnly)
	}
	if *verifyOnly {
		return UsageErrorf("--verify-only is only valid with --boundary (country packs already gate on sha256 during install; use --dry-run)")
	}
	cc, err := normaliseCountry(*country)
	if err != nil {
		return err
	}

	url := RegistryURL()
	fmt.Printf("==> registry: %s\n", url)
	reg, err := pack.FetchRegistry(ctx, url)
	if err != nil {
		return err
	}
	entry, ok := reg.Country(cc)
	if !ok {
		return UsageErrorf("country %s not in the registry — try `cockroach-cli pack list` to see what's published", cc)
	}
	if *version != "" && *version != entry.Version {
		// The registry exposes one "current" version per country today; pinning
		// a historical version means hitting GitHub Releases by tag. Out of
		// scope for v1 — surface the gap clearly rather than silently load
		// whatever's current.
		return UsageErrorf("requested version %q but registry currently exposes %q — pinned-version installs are not yet supported", *version, entry.Version)
	}

	db, err := openPool(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := requireInstalledPacksLedger(ctx, db); err != nil {
		return err
	}

	im := pack.NewInstaller(db)
	opts := pack.InstallOptions{
		DryRun:      *dryRun,
		InstalledBy: "cli",
		RegistryURL: url,
		FromDir:     *fromDir,
	}

	if *fromDir != "" {
		if !dirExists(*fromDir) {
			return UsageErrorf("--from-dir %q is not a directory\n  Try: point at a local pack dir, e.g. ../cockroach-india-seed/packs/%s", *fromDir, cc)
		}
		fmt.Printf("==> source: local dir %s (sha256/size still verified against the registry)\n", *fromDir)
	}

	fmt.Printf("==> installing %s (%s) — %d packs\n", entry.Name, entry.Version, len(entry.Packs))
	results, err := im.Install(ctx, cc, entry, opts)
	for _, r := range results {
		printResult(r, *dryRun)
	}
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Println("✓ dry-run: no rows written")
	} else {
		fmt.Printf("✓ %s installed (%d packs)\n", entry.Name, len(results))
	}
	return nil
}

// installLangPhase2Stub is the polite "not yet wired" exit for
// `cockroach-cli pack install --lang <code>`. Returns a usage error so the
// dispatcher exits 2 (same code, same meaning) — CI / automation can
// distinguish "language packs not yet available via pack install — fall back to
// lang import" from a genuine command failure (exit 1). The registry entry, if
// present, is summarised so the operator can still see what's catalogued.
//
// See plan/pack-cli-and-registry.md follow-up #3 and TODO.md's
// "Architecture asymmetry to address (Phase 2)" section for the migration plan.
func installLangPhase2Stub(ctx context.Context, code string) error {
	code = normaliseLang(code)
	url := RegistryURL()
	// Best-effort fetch: surface registry metadata if we can, but never let a
	// registry-fetch failure swallow the redirect — the operator's next step is
	// the same either way.
	if reg, err := pack.FetchRegistry(ctx, url); err == nil {
		if l, ok := reg.Lang(code); ok {
			fmt.Printf("==> registry: %s\n", url)
			fmt.Printf("%s — %s", code, l.Name)
			if l.NativeName != "" {
				fmt.Printf(" (%s)", l.NativeName)
			}
			fmt.Printf("  version %s, %d bundle(s)\n", l.Version, len(l.Bundles))
		}
	}
	return UsageErrorf(
		"PHASE-2 NOT YET AVAILABLE — `pack install --lang` is not yet wired to a download path.\n"+
			"Community UI translations still ship via the cockroach-world-language sibling-repo flow today.\n"+
			"To import this language into a running instance, run:\n"+
			"  cockroach-cli lang import --lang %s \\\n"+
			"    --instance https://your-instance.org --token \"$ADMIN_TOKEN\"\n"+
			"Phase-2 plan + tracking: plan/pack-cli-and-registry.md follow-up #3 (TODO.md \"Architecture asymmetry to address\").",
		code,
	)
}

// installBoundary runs the boundary install end-to-end: resolve the registry
// entry → fetch each asset → sha256-verify → parse GeoJSON → UPSERT into
// admin_units → record in installed_boundary_packs. LOUD ABORT on any sha256
// or size mismatch; nothing partial is ever written.
//
// --dry-run: fetch + verify + parse, print plan, write nothing.
// --verify-only: fetch + sha256-verify only; skip parse and write.
func installBoundary(ctx context.Context, code string, dryRun, verifyOnly bool) error {
	if dryRun && verifyOnly {
		return UsageErrorf("--dry-run and --verify-only are mutually exclusive")
	}
	cc, err := normaliseCountry(code)
	if err != nil {
		return err
	}
	url := RegistryURL()
	fmt.Printf("==> registry: %s\n", url)
	reg, err := pack.FetchRegistry(ctx, url)
	if err != nil {
		return err
	}
	b, ok := reg.Boundary(cc)
	if !ok {
		return UsageErrorf("boundary %s not in the registry — try `cockroach-cli pack list --boundaries` to see what's published", cc)
	}

	fmt.Printf("==> installing %s — %s (%s)\n", cc, b.Name, b.Version)
	fmt.Printf("  license  : %s\n", firstNonEmpty(b.License, "(unspecified)"))
	if b.Notes != "" {
		fmt.Printf("  notes    : %s\n", b.Notes)
	}

	// Verify-only mode skips the DB pool entirely — purely a network + sha
	// integrity check. Mirrors `cockroach-cli pack verify --boundary` but reuses
	// the installer's per-asset walker so the multi-file bundles get covered
	// uniformly.
	var db *pgxpoolFacade
	if !verifyOnly {
		pool, err := openPool(ctx)
		if err != nil {
			return err
		}
		defer pool.Close()
		if !dryRun {
			if err := requireBoundaryLedger(ctx, pool); err != nil {
				return err
			}
		}
		db = wrapPool(pool)
	}

	bi := &pack.BoundaryInstaller{Client: nil}
	if db != nil {
		bi.DB = db.pool
	}
	opts := pack.BoundaryInstallOptions{
		DryRun:      dryRun,
		VerifyOnly:  verifyOnly,
		InstalledBy: "cli",
		RegistryURL: url,
	}
	res, err := bi.InstallBoundary(ctx, cc, b, opts)
	for _, a := range res.Assets {
		printBoundaryAssetResult(a, dryRun, verifyOnly)
	}
	if err != nil {
		return err
	}
	if res.Skipped {
		fmt.Printf("• %s already at sha256 %s — nothing to do\n", cc, short12(res.SHA256))
		return nil
	}
	switch {
	case verifyOnly:
		fmt.Printf("✓ %s verified — %d asset(s) match the registry\n", cc, len(res.Assets))
	case dryRun:
		fmt.Println("✓ dry-run: no rows written to admin_units; installed_boundary_packs not updated")
	default:
		var rows int64
		for _, a := range res.Assets {
			rows += a.RowsUpserted
		}
		fmt.Printf("✓ %s installed — %d admin_units row(s) across %d asset(s)\n", cc, rows, len(res.Assets))
	}
	return nil
}

// printBoundaryAssetResult renders one asset's plan / verify / install line.
func printBoundaryAssetResult(a pack.BoundaryAssetResult, dryRun, verifyOnly bool) {
	switch {
	case verifyOnly:
		fmt.Printf("  ✓ %-12s  %s  %s  (%dms)\n",
			a.Role, humanBytes(a.BytesFetched), short12(a.SHA256), a.DurationMs)
	case dryRun:
		fmt.Printf("  • %-12s  features=%d  %s  %s  (%dms)\n",
			a.Role, a.FeatureCount, humanBytes(a.BytesFetched), short12(a.SHA256), a.DurationMs)
	default:
		fmt.Printf("  ✓ %-12s  features=%d  upserted=%d  %s  (%dms)\n",
			a.Role, a.FeatureCount, a.RowsUpserted, humanBytes(a.BytesFetched), a.DurationMs)
	}
}

// requireBoundaryLedger fails loudly when migration 078 hasn't been applied.
// Same pattern as requireInstalledPacksLedger — better to surface a clear "run
// cockroach-cli migrate first" than crash mid-install with a confusing
// "relation installed_boundary_packs does not exist".
func requireBoundaryLedger(ctx context.Context, db *pgxpool.Pool) error {
	var ledger, units bool
	if err := db.QueryRow(ctx,
		`SELECT to_regclass('public.installed_boundary_packs') IS NOT NULL,
		        to_regclass('public.admin_units') IS NOT NULL`,
	).Scan(&ledger, &units); err != nil {
		return fmt.Errorf("probe boundary tables: %w", err)
	}
	if !ledger || !units {
		return fmt.Errorf("installed_boundary_packs / admin_units missing — run `cockroach-cli migrate` first (need migration 078)")
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// pgxpoolFacade lets us keep the install code's DB-handle plumbing tidy
// without pulling pgxpool into a million import sites.
type pgxpoolFacade struct{ pool *pgxpool.Pool }

func wrapPool(p *pgxpool.Pool) *pgxpoolFacade { return &pgxpoolFacade{pool: p} }

// ── verify ────────────────────────────────────────────────────────────────────

func cmdPackVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pack verify", flag.ContinueOnError)
	country := fs.String("country", "", "ISO 3166-1 alpha-2 country code (e.g. PS)")
	lang := fs.String("lang", "", "ISO 639-1/3 language code (e.g. hi). Phase-2 — metadata-only verify.")
	boundary := fs.String("boundary", "", "ISO 3166-1 alpha-2 country code for the registered boundary GeoJSON (e.g. IN).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !exactlyOne(*country != "", *lang != "", *boundary != "") {
		return UsageErrorf("provide exactly one of --country <cc>, --lang <code>, or --boundary <cc>")
	}
	if *lang != "" {
		return verifyLangPhase2(ctx, *lang)
	}
	if *boundary != "" {
		return verifyBoundary(ctx, *boundary)
	}
	cc, err := normaliseCountry(*country)
	if err != nil {
		return err
	}
	reg, err := pack.FetchRegistry(ctx, RegistryURL())
	if err != nil {
		return err
	}
	entry, ok := reg.Country(cc)
	if !ok {
		return UsageErrorf("country %s not in the registry", cc)
	}

	// verify = install --dry-run without touching the DB at all. We still need
	// a DB pool for the ledger short-circuit; pass nil to force a fetch+hash
	// for every pack, surfacing any registry-vs-bytes drift.
	im := &pack.Installer{}
	fmt.Printf("==> verifying %s (%s)\n", entry.Name, entry.Version)
	for _, p := range entry.Packs {
		started := time.Now()
		// Hand-roll: download + sha check, no DB.
		res, err := im.InstallPack(ctx, cc, entry.Version, p, pack.InstallOptions{DryRun: true}, entry)
		dur := time.Since(started)
		if err != nil {
			fmt.Printf("  ✗ %-32s  %s\n", p.File, err)
			return err
		}
		fmt.Printf("  ✓ %-32s  rows=%d  %s  (%dms)\n",
			p.File, res.RowsInCSV, humanBytes(res.BytesFetched), dur.Milliseconds())
	}
	fmt.Println("✓ all packs verified against the registry")
	return nil
}

// verifyLangPhase2 surfaces the registry entry for a language pack and prints a
// "not yet downloadable" footer. The actual sha256-against-bytes check is
// Phase-2 work — until the cockroach-world-language CI publishes bundles + a
// real download path is wired through `pkg/pack`, "verify" can only confirm the
// registry shape parses and the entry exists.
func verifyLangPhase2(ctx context.Context, code string) error {
	code = normaliseLang(code)
	reg, err := pack.FetchRegistry(ctx, RegistryURL())
	if err != nil {
		return err
	}
	l, ok := reg.Lang(code)
	if !ok {
		return UsageErrorf("language %q not in the registry — see `cockroach-cli pack list --langs` (Phase-2 — community translations still ship via `cockroach-cli lang import` for now)", code)
	}
	fmt.Printf("==> verifying %s — %s (%s)\n", code, l.Name, l.Version)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  APP\tFILE\tSIZE\tSHA256")
	for _, b := range l.Bundles {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s…\n",
			b.App, b.File, humanBytes(b.SizeBytes), short12(b.SHA256))
	}
	tw.Flush()
	fmt.Println("✓ registry entry parsed and metadata present")
	fmt.Println()
	fmt.Println("Phase-2: bundle sha256-against-bytes verification is not yet wired.")
	fmt.Printf("Use `cockroach-cli lang import --lang %s` (sibling-repo flow) to import translations today.\n", code)
	return nil
}

// verifyBoundary HEADs the registry's boundary GeoJSON URL, then GETs the body
// when the size is reasonable, and sha256-verifies it against the registry's
// pin. Mirror of `pack verify --country`'s contract — no DB, no writes.
//
// Bytes cap: we refuse to slurp anything past boundaryVerifyMaxBytes to keep a
// misconfigured / hijacked URL from wedging the CLI.
const boundaryVerifyMaxBytes = 64 << 20 // 64 MiB

func verifyBoundary(ctx context.Context, code string) error {
	cc, err := normaliseCountry(code)
	if err != nil {
		return err
	}
	reg, err := pack.FetchRegistry(ctx, RegistryURL())
	if err != nil {
		return err
	}
	b, ok := reg.Boundary(cc)
	if !ok {
		return UsageErrorf("boundary %s not in the registry — see `cockroach-cli pack list --boundaries`", cc)
	}
	fmt.Printf("==> verifying %s — %s (%s)\n", cc, b.Name, b.Version)
	fmt.Printf("  url      : %s\n", b.URL)
	fmt.Printf("  expected : sha256=%s…  size=%s\n", short12(b.SHA256), humanBytes(b.SizeBytes))

	// HEAD first — surfaces 404/403/redirects early without paying for the body.
	headCtx, headCancel := context.WithTimeout(ctx, 30*time.Second)
	defer headCancel()
	headReq, err := http.NewRequestWithContext(headCtx, http.MethodHead, b.URL, nil)
	if err != nil {
		return fmt.Errorf("build HEAD request: %w", err)
	}
	headReq.Header.Set("User-Agent", "cockroach-cli/pack")
	headResp, err := http.DefaultClient.Do(headReq)
	if err != nil {
		return fmt.Errorf("HEAD %s: %w", b.URL, err)
	}
	_ = headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		return fmt.Errorf("HEAD %s returned HTTP %d", b.URL, headResp.StatusCode)
	}
	if cl := headResp.ContentLength; cl > 0 && cl != b.SizeBytes {
		fmt.Printf("  ! HEAD Content-Length=%d differs from registry size_bytes=%d\n", cl, b.SizeBytes)
	}

	// Bail out before the GET if the upstream advertises a size beyond the cap.
	if cl := headResp.ContentLength; cl > boundaryVerifyMaxBytes {
		return fmt.Errorf("boundary %s advertises %s (Content-Length) which exceeds verify cap of %s — refusing to download",
			cc, humanBytes(cl), humanBytes(boundaryVerifyMaxBytes))
	}

	getCtx, getCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer getCancel()
	getReq, err := http.NewRequestWithContext(getCtx, http.MethodGet, b.URL, nil)
	if err != nil {
		return fmt.Errorf("build GET request: %w", err)
	}
	getReq.Header.Set("User-Agent", "cockroach-cli/pack")
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		return fmt.Errorf("GET %s: %w", b.URL, err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned HTTP %d", b.URL, getResp.StatusCode)
	}

	started := time.Now()
	h := sha256.New()
	// LimitReader is a guardrail; a body bigger than the cap will stream into
	// the hasher but be truncated, surfacing as a digest mismatch (correct
	// failure mode — we want to refuse oversized payloads).
	n, err := io.Copy(h, io.LimitReader(getResp.Body, boundaryVerifyMaxBytes+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	dur := time.Since(started)
	if n > boundaryVerifyMaxBytes {
		return fmt.Errorf("boundary %s body exceeded verify cap of %s — refusing to verify partial download",
			cc, humanBytes(boundaryVerifyMaxBytes))
	}
	if n != b.SizeBytes {
		return fmt.Errorf("size mismatch: got %d bytes, registry pins %d", n, b.SizeBytes)
	}
	got := hex.EncodeToString(h.Sum(nil))
	want := strings.ToLower(strings.TrimSpace(b.SHA256))
	if got != want {
		return fmt.Errorf("sha256 mismatch:\n  got    %s\n  expect %s", got, want)
	}
	fmt.Printf("  ✓ %s bytes  sha256=%s…  (%dms)\n", humanBytes(n), short12(got), dur.Milliseconds())
	fmt.Println("✓ boundary verified against the registry")
	return nil
}

// ── search ────────────────────────────────────────────────────────────────────

func cmdPackSearch(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return UsageErrorf("usage: cockroach-cli pack search <query>")
	}
	q := strings.ToLower(strings.Join(args, " "))
	reg, err := pack.FetchRegistry(ctx, RegistryURL())
	if err != nil {
		return err
	}
	matched := 0
	for _, cc := range reg.CountryCodes() {
		c, _ := reg.Country(cc)
		if !strings.Contains(strings.ToLower(cc), q) && !strings.Contains(strings.ToLower(c.Name), q) {
			continue
		}
		fmt.Printf("%s  %s  (v%s, %d packs)\n", cc, c.Name, c.Version, len(c.Packs))
		matched++
	}
	if matched == 0 {
		return fmt.Errorf("no countries matched %q (try `cockroach-cli pack list`)", q)
	}
	return nil
}

// ── shared ────────────────────────────────────────────────────────────────────

func openPool(ctx context.Context) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(DSN())
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w\n  Try: export DATABASE_URL=postgres://user:pass@host:port/db?sslmode=disable", err)
	}
	cfg.MaxConns = 4
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w\n  Try: check the Postgres host is up and credentials match (`make dev-infra` for the dev stack)", err)
	}
	return db, nil
}

// requireInstalledPacksLedger fails loudly when migration 069 hasn't been
// applied — otherwise the very first install would crash mid-flow with a
// confusing "relation installed_packs does not exist".
func requireInstalledPacksLedger(ctx context.Context, db *pgxpool.Pool) error {
	var present bool
	if err := db.QueryRow(ctx,
		`SELECT to_regclass('public.installed_packs') IS NOT NULL`,
	).Scan(&present); err != nil {
		return fmt.Errorf("probe installed_packs: %w", err)
	}
	if !present {
		return fmt.Errorf("installed_packs table missing — run `cockroach-cli migrate` first")
	}
	return nil
}

func printResult(r pack.PackResult, dryRun bool) {
	switch {
	case r.Skipped:
		fmt.Printf("  • %-32s  skip  (%s)\n", r.Pack.File, r.SkipReason)
	case dryRun:
		fmt.Printf("  • %-32s  plan  rows=%d  %s\n",
			r.Pack.File, r.RowsInCSV, humanBytes(r.BytesFetched))
	default:
		fmt.Printf("  ✓ %-32s  rows=%d  inserted=%d  (%dms)\n",
			r.Pack.File, r.RowsInCSV, r.RowsInserted, r.DurationMs)
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func short12(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
