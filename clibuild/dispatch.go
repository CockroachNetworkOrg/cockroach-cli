package clibuild

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"
)

// Dispatch runs the command matching args[0] from cmds and returns the process
// exit code: 0 ok, 1 runtime error, 2 usage error. It is the single dispatch
// loop both the standalone and product CLIs share — they differ only in which
// commands they pass in and in their own top-level --help / version banners
// (handled by the caller's main() before/around this).
//
// The contract:
//   - flag.ErrHelp from a subcommand → exit 0 (the flagset already printed Usage).
//   - an error unwrapping to ErrUsage → exit 2.
//   - any other error → exit 1.
//
// Unknown command → exit 2; the caller is expected to have already handled the
// help/version top-level switch, so Dispatch only owns the command table.
func Dispatch(progName string, cmds []Command, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "%s: no command given\n", progName)
		return 2
	}
	sub := args[0]
	for _, c := range cmds {
		if c.Name != sub {
			continue
		}
		// A long migrate run shouldn't be aborted by a tight timeout, so give
		// the context a generous backstop; per-command logic can tighten it.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := c.Run(ctx, args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", progName, sub, err)
			if errors.Is(err, ErrUsage) {
				return 2
			}
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "%s: unknown command %q\n", progName, sub)
	return 2
}
