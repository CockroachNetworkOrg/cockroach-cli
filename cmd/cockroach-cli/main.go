// Command cockroach-cli is the standalone, cross-product CLI for the Cockroach
// Network ecosystem. It exposes the genuinely product-agnostic surface — the
// country data-pack package manager and the UI-translation delegator — without
// dragging in any single product's operational machinery.
//
// It is DELIBERATELY thin: the cobra command tree, the exit-code contract, and
// every CORE command (version, pack, lang) live in the importable clibuild
// package, which the reporters product CLI builds from too. This binary just
// runs clibuild.RootCommand().Execute() and maps the result to the exit code.
//
//	cockroach-cli version                          print the build identity
//	cockroach-cli pack list [--langs|--boundaries|--all|--installed]
//	cockroach-cli pack info    --country|--lang|--boundary <code>
//	cockroach-cli pack install --country|--boundary <code> [--version|--from-dir|--dry-run|--verify-only]
//	cockroach-cli pack verify  --country|--lang|--boundary <code>
//	cockroach-cli pack search  <query>
//	cockroach-cli lang <enable|disable|import|update|list|stats>
//	cockroach-cli completion <bash|zsh|fish|powershell>
//
// Env:
//
//	COCKROACH_REGISTRY_URL    override the default catalog URL (forks / mirrors / air-gapped)
//	COCKROACH_PACK_DIR        default for `pack install --from-dir`
//	DATABASE_URL              target Postgres DSN (required for install + --installed)
//	LANG_SEED_DIR             override the sibling cockroach-world-language checkout
//
// Built on spf13/cobra: the command tree + shell completions are cobra's; each
// command keeps its own flag parsing, output, and the 0/1/2 exit-code contract.
package main

import (
	"fmt"
	"os"

	"github.com/cockroachnetworkorg/cockroach-cli/clibuild"
	"github.com/cockroachnetworkorg/cockroach-cli/internal/version"
)

func main() {
	prog := clibuild.ProgName()
	// `version` is ALSO a core command (so it works through the cobra tree), but
	// the bare-flag aliases --version / -v stay top-level conveniences.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "--version", "-v":
			fmt.Printf("%s — Cockroach Network ecosystem CLI\n", prog)
			fmt.Printf("Build   : %s\n", version.Full())
			fmt.Println("Project : https://cockroachnetwork.org")
			fmt.Println("Source  : https://github.com/cockroachnetworkorg/cockroach-cli")
			fmt.Println("License : Apache-2.0")
			return
		}
	}
	root := clibuild.RootCommand()
	root.SetArgs(os.Args[1:])
	err := root.Execute()
	if err != nil {
		// flag.ErrHelp is silent (the subcommand already printed its --help);
		// every other error is reported on stderr before the exit-code mapping.
		if code := clibuild.ExitCode(err); code != 0 {
			fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		}
	}
	os.Exit(clibuild.ExitCode(err))
}
