package clibuild

import (
	"context"
	"fmt"

	"github.com/cockroachnetworkorg/cockroach-cli/internal/version"
)

// CoreCommands returns the cross-product CORE command set both the standalone
// cockroach-cli binary and the reporters product CLI are built from: the
// version banner plus the country data-pack manager (pack family, incl.
// upgrade) and the UI-translation delegator (lang). Product CLIs append their
// own product-specific commands to this slice before handing it to Dispatch.
//
// pack-upgrade is folded into `pack upgrade` (CmdPack routes the `upgrade`
// subcommand to CmdPackUpgrade), so it is core too — there is no reporters
// coupling in the upgrade path; it reads the same installed_packs ledger +
// registry as the rest of the pack family.
func CoreCommands() []Command {
	return []Command{
		{"version", "Print the build identity (version, commit, build date)", cmdVersion},
		{"pack", "Manage country data packs (list, install, info, verify, search, upgrade)", CmdPack},
		{"lang", "Manage UI translations + locales (enable, disable, import, update, list, stats)", CmdLang},
	}
}

// cmdVersion prints the standalone build identity. The reporters product CLI
// keeps its OWN top-level version banner (it versions on the reporters repo,
// not this module), so this core entry is what the STANDALONE binary surfaces.
func cmdVersion(_ context.Context, _ []string) error {
	fmt.Printf("%s — Cockroach Network ecosystem CLI\n", ProgName())
	fmt.Printf("Build   : %s\n", version.Full())
	fmt.Println("Project : https://cockroachnetwork.org")
	fmt.Println("Source  : https://github.com/cockroachnetworkorg/cockroach-cli")
	fmt.Println("License : Apache-2.0")
	return nil
}
