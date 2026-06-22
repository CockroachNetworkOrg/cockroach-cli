package clibuild

// `cockroach-cli pack upgrade` — the `apt list --upgradable` + `apt upgrade` pair
// for country reference packs. Compares each row in the installed_packs ledger
// against the version currently published in the registry, prints the diff,
// and (when --apply / -y is passed) re-runs the existing pack installer for
// the rows that drifted.
//
// Design contract:
//
//   - SAFE BY DEFAULT. Without --apply / -y the command prints the table and
//     exits 0. Operators see the plan before any write hits Postgres.
//   - REUSES INSTALL LOGIC. The actual upgrade is `pack.Installer.Install` on
//     the same CountryEntry the registry just resolved. We do NOT reimplement
//     the COPY / UPSERT loop — that lives in `pkg/pack` and stays the single
//     source of truth.
//   - DRY-RUN PROPAGATES. `--dry-run` forwards to the installer's DryRun
//     option, so a dry-run upgrade fetches + verifies bytes without writing.
//
// Output columns (plan view): COUNTRY, FILE, CURRENT → AVAILABLE, SHA256?,
// ROWS-EST. SHA256? answers "did the digest change". ROWS-EST is the registry's
// expected_rows for the new pack; a forward estimate, not a delta.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/cockroachnetworkorg/cockroach-cli/pkg/pack"
)

// upgradeCandidate is one row of the upgrade plan — a pack that's installed
// and whose registry counterpart has a different version or sha256.
type upgradeCandidate struct {
	CountryCode    string
	PackFile       string
	CurrentVersion string
	NewVersion     string
	CurrentSHA256  string
	NewSHA256      string
	RowsEstimate   int64
}

// SHA256Changed reports whether the digest moved between the installed row and
// the registry row. A version bump with no sha256 change usually means the
// publisher republished the same bytes under a new label.
func (u upgradeCandidate) SHA256Changed() bool {
	return !strings.EqualFold(u.CurrentSHA256, u.NewSHA256)
}

// CmdPackUpgrade is exported so the `pack upgrade` subcommand can be reached
// from CmdPack and (potentially) a product CLI directly.
func CmdPackUpgrade(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pack upgrade", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "fetch + verify only; write nothing (forwarded to the installer)")
	apply := fs.Bool("apply", false, "actually run the upgrade (without this flag the plan is printed and nothing is written)")
	yes := fs.Bool("y", false, "shorthand for --apply")
	country := fs.String("country", "", "limit the plan to one country (ISO 3166-1 alpha-2)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: cockroach-cli pack upgrade [--country <cc>] [--dry-run] [--apply | -y]

Compare every installed pack against the registry's current version and print
the outdated list. Without --apply / -y, this is a read-only preview (like
`+"`apt list --upgradable`"+`); with --apply, it re-runs the in-process pack
installer for each outdated row (like `+"`apt upgrade`"+`).

Columns:
  COUNTRY   ISO 3166-1 alpha-2
  FILE      one csv.gz per installed pack row
  CURRENT   the version the installed_packs ledger has
  AVAILABLE the version the registry publishes today
  SHA256?   "changed" when the digest moved, "same" otherwise
  ROWS-EST  the registry's expected_rows for the new pack (informational)

Examples:
  cockroach-cli pack upgrade                     # plan only (safe; default)
  cockroach-cli pack upgrade --country IN        # plan for one country
  cockroach-cli pack upgrade --apply             # run the upgrade
  cockroach-cli pack upgrade --dry-run --apply   # fetch + verify only, no writes

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	// --apply is the explicit confirmation; -y is the conventional shorthand.
	doApply := *apply || *yes

	scope := ""
	if *country != "" {
		cc, err := normaliseCountry(*country)
		if err != nil {
			return err
		}
		scope = cc
	}

	db, err := openPool(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := requireInstalledPacksLedger(ctx, db); err != nil {
		return err
	}

	installed, err := pack.ListInstalled(ctx, db)
	if err != nil {
		return fmt.Errorf("list installed: %w", err)
	}
	if scope != "" {
		installed = filterByCountry(installed, scope)
	}
	if len(installed) == 0 {
		fmt.Println("no packs installed — nothing to upgrade")
		fmt.Println("(run `cockroach-cli pack install --country <cc>` first)")
		return nil
	}

	url := RegistryURL()
	fmt.Printf("==> registry: %s\n", url)
	reg, err := pack.FetchRegistry(ctx, url)
	if err != nil {
		return err
	}

	plan, registryByCountry := computeUpgradePlan(installed, reg)
	if scope != "" {
		// Drop registry entries we don't need so the apply loop doesn't fetch
		// extra countries' bytes.
		for cc := range registryByCountry {
			if cc != scope {
				delete(registryByCountry, cc)
			}
		}
	}

	printUpgradePlan(os.Stdout, plan)

	if len(plan) == 0 {
		return nil
	}
	if !doApply {
		fmt.Println()
		fmt.Println("(plan only — pass --apply or -y to run the upgrade)")
		return nil
	}

	// Apply — group the candidates by country and re-run the installer for
	// each. We reuse the installer's per-pack short-circuit (already at
	// sha256) so any rows that aren't actually outdated re-skip cheaply.
	im := pack.NewInstaller(db)
	opts := pack.InstallOptions{
		DryRun:      *dryRun,
		InstalledBy: "cli-upgrade",
		RegistryURL: url,
	}
	for cc, entry := range registryByCountry {
		fmt.Printf("==> upgrading %s (%s) — %d pack(s)\n", entry.Name, entry.Version, len(entry.Packs))
		results, err := im.Install(ctx, cc, entry, opts)
		for _, r := range results {
			printResult(r, *dryRun)
		}
		if err != nil {
			return fmt.Errorf("upgrade %s: %w", cc, err)
		}
	}
	if *dryRun {
		fmt.Println("✓ dry-run: no rows written")
	} else {
		fmt.Println("✓ upgrade complete")
	}
	return nil
}

// computeUpgradePlan compares the installed ledger against the registry and
// returns (a) the outdated candidates in display order and (b) a per-country
// CountryEntry map so the apply step can re-run the installer without
// re-fetching the registry.
func computeUpgradePlan(installed []pack.InstalledPack, reg *pack.Registry) ([]upgradeCandidate, map[string]pack.CountryEntry) {
	out := []upgradeCandidate{}
	involved := map[string]pack.CountryEntry{}
	for _, ip := range installed {
		entry, ok := reg.Country(ip.CountryCode)
		if !ok {
			// Country isn't in the registry anymore — skip the row. The
			// operator can still see it via `cockroach-cli pack list --installed`.
			continue
		}
		// Find the matching pack file in the registry's pack list.
		var match pack.Pack
		found := false
		for _, p := range entry.Packs {
			if p.File == ip.PackFile {
				match = p
				found = true
				break
			}
		}
		if !found {
			// The registry dropped this pack file. Don't propose an upgrade —
			// removal is a different operation than upgrade.
			continue
		}
		if entry.Version == ip.Version && strings.EqualFold(match.SHA256, ip.SHA256) {
			// Up to date.
			continue
		}
		out = append(out, upgradeCandidate{
			CountryCode:    ip.CountryCode,
			PackFile:       ip.PackFile,
			CurrentVersion: ip.Version,
			NewVersion:     entry.Version,
			CurrentSHA256:  ip.SHA256,
			NewSHA256:      match.SHA256,
			RowsEstimate:   match.ExpectedRows,
		})
		involved[ip.CountryCode] = entry
	}
	return out, involved
}

func filterByCountry(in []pack.InstalledPack, cc string) []pack.InstalledPack {
	out := make([]pack.InstalledPack, 0, len(in))
	for _, p := range in {
		if p.CountryCode == cc {
			out = append(out, p)
		}
	}
	return out
}

// printUpgradePlan renders the plan table. Mirrors `apt list --upgradable`
// — one row per outdated pack, with a clear "nothing to do" line on empty.
func printUpgradePlan(w *os.File, plan []upgradeCandidate) {
	if len(plan) == 0 {
		fmt.Fprintln(w, "All installed packs are up to date.")
		return
	}
	fmt.Fprintf(w, "%d pack(s) can be upgraded:\n", len(plan))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  COUNTRY\tFILE\tCURRENT\t→\tAVAILABLE\tSHA256?\tROWS-EST")
	for _, c := range plan {
		shaTag := "same"
		if c.SHA256Changed() {
			shaTag = "changed"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t→\t%s\t%s\t%d\n",
			c.CountryCode, c.PackFile, c.CurrentVersion, c.NewVersion, shaTag, c.RowsEstimate)
	}
	tw.Flush()
}
