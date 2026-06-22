package clibuild

// Cobra command layer.
//
// The CLI's commands (version, pack, lang — and, in the reporters product
// build, ~16 more) were authored against the stdlib `flag` package: each one is
// a `func(ctx, args) error` that builds its own flag.FlagSet, prints its own
// hand-tuned --help, and signals usage problems via the ErrUsage sentinel. That
// per-command machinery is GOOD and well-tested; we do not want to rewrite it.
//
// So cobra is adopted as the *command tree + extension seam* (the kubectl/gh/helm
// shape) WITHOUT taking over per-command flag parsing. Each leaf cobra command
// runs with DisableFlagParsing=true and hands its raw args straight to the
// existing `func(ctx, args) error`. The result:
//
//   - `RootCommand()` is a real *cobra.Command, so a product binary extends the
//     CLI with `root.AddCommand(...)` (the clean seam the migration was for).
//   - cobra contributes `completion` (bash/zsh/fish/pwsh) and a root-level
//     command listing for free.
//   - EVERY command's own flags, output, --help text, and the 0/1/2 exit-code
//     contract are preserved byte-for-byte — they still flow through the exact
//     same flag.FlagSet + ErrUsage paths they always did.
//
// The exit-code contract is enforced by ExitCode(err) below, which the thin
// main()s call: flag.ErrHelp → 0, ErrUsage → 2, any other error → 1, nil → 0.
// Unknown command / no command → cobra returns an error we map to 2.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// RunFunc is the historic command signature every core + product command is
// written against: it receives the args AFTER the command name and returns a
// non-nil error on failure (ErrUsage-wrapped for a usage problem).
type RunFunc func(ctx context.Context, args []string) error

// DelegatingCommand builds a leaf *cobra.Command that hands its raw args to an
// existing RunFunc, preserving that command's own flag parsing / --help / exit
// behaviour. use is the one-line usage token (e.g. "pack" or "pack [flags]");
// short is the registry summary shown in the parent's command list.
//
// DisableFlagParsing is the crux: cobra does NOT touch the args, so `--help`,
// `-h`, unknown flags, `--`, and every existing flag.FlagSet token reach the
// RunFunc exactly as before. We therefore also suppress cobra's own help so a
// bare `<cmd> --help` is handled by the RunFunc's hand-tuned usage, not cobra's.
func DelegatingCommand(name, short string, run RunFunc) *cobra.Command {
	c := &cobra.Command{
		Use:                name,
		Short:              short,
		DisableFlagParsing: true,
		// Args reach RunE untouched; the command parses them itself.
		RunE: func(cmd *cobra.Command, args []string) error {
			// A long migrate/seed run shouldn't be aborted by a tight timeout, so
			// give the context a generous backstop; per-command logic can tighten
			// it. (Mirrors the historic Dispatch behaviour.)
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
			defer cancel()
			return run(ctx, args)
		},
		// The RunFunc owns help; don't let cobra print its own usage on error.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	return c
}

// newRoot builds the shared root with cobra's house settings applied. progName
// is the invoked binary name (ProgName()), so help/usage echo how the tool was
// actually called — preserving the busybox/git argv[0] convention.
func newRoot(progName, short string) *cobra.Command {
	root := &cobra.Command{
		Use:   progName,
		Short: short,
		// Exit codes are owned by ExitCode(err) in main(); cobra must not print
		// its own error/usage noise or we'd double-report and lose the contract.
		SilenceUsage:  true,
		SilenceErrors: true,
		// A bare invocation with no subcommand is a usage error (exit 2), matching
		// the historic `no command given` behaviour.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return UsageErrorf("no command given")
			}
			return UsageErrorf("unknown command %q", args[0])
		},
	}
	// A flag-parse error at the root (e.g. an unknown global flag) is a usage
	// problem → exit 2, not a runtime error. Wrap it in the ErrUsage sentinel.
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return UsageErrorf("%v", err)
	})
	// completion is the one genuinely-new capability cobra brings; keep it.
	root.AddCommand(completionCommand(progName))
	return root
}

// isCobraUsageError reports whether err is one of cobra's own structural
// command errors (unknown command / unknown subcommand / missing required
// subcommand) that should map to exit 2. Cobra returns these as plain errors
// without a typed sentinel, so we match the stable message shapes it emits.
func isCobraUsageError(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.HasPrefix(m, "unknown command") ||
		strings.HasPrefix(m, "unknown subcommand") ||
		strings.Contains(m, "unknown flag") ||
		strings.Contains(m, "unknown shorthand flag") ||
		strings.Contains(m, "invalid argument") ||
		strings.Contains(m, "requires at least") ||
		strings.Contains(m, "accepts ")
}

// RootCommand returns the CORE cobra root the standalone cockroach-cli binary
// runs and the reporters product binary EXTENDS via root.AddCommand(...). It
// carries the cross-product core commands (version, pack, lang) plus cobra's
// `completion`. Product CLIs append their own *cobra.Command children.
func RootCommand() *cobra.Command {
	root := newRoot(ProgName(), "Cockroach Network ecosystem CLI")
	for _, c := range CoreCommands() {
		root.AddCommand(DelegatingCommand(c.Name, c.Summary, c.Run))
	}
	return root
}

// completionCommand is cobra's standard shell-completion generator, exposed as a
// `completion <bash|zsh|fish|powershell>` subcommand. This is the headline DX
// win of the cobra migration (kubectl/gh-style completions).
func completionCommand(progName string) *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate a shell completion script (bash, zsh, fish, powershell)",
		Long: fmt.Sprintf(`Generate a shell completion script for %[1]s.

Load it for the current session, or install it permanently per your shell:

  # bash (current shell)
  source <(%[1]s completion bash)
  # zsh
  source <(%[1]s completion zsh)
  # fish
  %[1]s completion fish | source
  # powershell
  %[1]s completion powershell | Out-String | Invoke-Expression
`, progName),
		Args:                  cobra.ExactValidArgs(1),
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		DisableFlagsInUseLine: true,
		SilenceUsage:          true,
		SilenceErrors:         true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(cmd.OutOrStdout(), true)
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			case "fish":
				return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
			default:
				return UsageErrorf("unknown shell %q (want: bash | zsh | fish | powershell)", args[0])
			}
		},
	}
}

// ExitCode maps a command error to the process exit code, preserving the exact
// contract the hand-rolled Dispatch enforced:
//
//	nil               → 0   success
//	flag.ErrHelp      → 0   a subcommand already printed its --help
//	wraps ErrUsage    → 2   usage error (bad args, missing flag, unknown command)
//	any other error   → 1   runtime error
//
// The thin main()s call this on the result of root.Execute(). Errors are printed
// by the caller (root has SilenceErrors), except flag.ErrHelp which is silent.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if errors.Is(err, ErrUsage) {
		return 2
	}
	// Cobra's own unknown-command / bad-args errors are usage problems too.
	if isCobraUsageError(err) {
		return 2
	}
	return 1
}
