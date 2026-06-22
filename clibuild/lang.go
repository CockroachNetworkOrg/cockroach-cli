package clibuild

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// cmdLang manages UI translations by delegating to the sibling
// `cockroach-world-language` checkout's `cw-lang` CLI — translations are NOT
// vendored into the framework. The community-maintained language repo pushes
// per-namespace bundles into a running instance's DB-driven i18n store via the
// admin import API; this command just locates the checkout and runs it.
//
// The cw-lang-backed subcommands take a target + forwarded flags:
//
//	cockroach-cli lang import --country <cc> [extra cw-lang flags…]
//
// Extra flags after the recognised ones are forwarded verbatim to cw-lang
// (e.g. --instance, --token, --apps, --dry-run), so the full cw-lang surface is
// reachable without re-declaring every flag here.
//
// `enable` / `disable` are the exception: they do NOT shell out to cw-lang and
// do NOT need a running server + admin token. They talk DIRECTLY to
// $DATABASE_URL (the same pgx path migrate / status use) to toggle the
// `ui_locales` row for a locale — the gap that previously forced operators
// through the admin HTTP API for a dev reseed or CI run:
//
//	cockroach-cli lang enable  <code> [<code>…]   upsert ui_locales row, enabled=TRUE
//	cockroach-cli lang disable <code> [<code>…]   set enabled=FALSE (strings preserved)
//
// Both are idempotent UPSERTs and never delete strings, so a disabled locale's
// translations stay put and re-enabling is instant. 'en' is rejected (it is the
// baked-in fallback, never a managed locale — mirrors the server's
// ErrLocaleIsEnglish).
//
// FUTURE — Phase-2 unification with `cockroach-cli pack`:
//
// The central registry.json schema already accepts `langs[<code>]` entries
// (parallel to `countries[<cc>]`) — see cockroachnetworkorg/cockroach-registry
// schema/registry.schema.json and the matching Lang / LangBundle types in
// backend/pkg/pack/registry.go. The CLI's `cockroach-cli pack` family now
// recognises `--lang <code>`:
//
//   - `cockroach-cli pack list --langs`              surfaces what's catalogued
//   - `cockroach-cli pack info  --lang <code>`       metadata for one language
//   - `cockroach-cli pack verify --lang <code>`      metadata-only verify
//   - `cockroach-cli pack install --lang <code>`     prints a PHASE-2 redirect to
//     this command and exits 2
//
// The Phase-2 work that's still pending is the download + admin-API push path
// itself: cockroach-world-language CI needs to publish per-language bundles
// (web / admin / mobile tarballs) as GitHub Release assets, pin sha256s into
// the central registry, and the framework's importer in `backend/pkg/pack`
// needs a language-bundle-equivalent of the country `.csv.gz` flow. Once that
// lands, `cockroach-cli pack install --lang` will replace this command and `lang
// import` will become a deprecated alias that prints a redirect (same shape
// `cockroach-cli data` used when it pointed operators at `cockroach-cli pack install
// --country`). Tracked as follow-up #3 in plan/pack-cli-and-registry.md.
//
// Exported so a product CLI can expose it directly (reporters' bootstrap/setup
// call it) and so CoreCommands can register it.
func CmdLang(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return UsageErrorf("usage: cockroach-cli lang <enable|disable|import|update|list|stats> [flags…]")
	}
	switch args[0] {
	case "enable":
		// Direct-to-DB: upsert the ui_locales row with enabled=TRUE.
		return cmdLangSetEnabled(ctx, args[1:], true)
	case "disable":
		// Direct-to-DB: flip enabled=FALSE; strings are NOT deleted.
		return cmdLangSetEnabled(ctx, args[1:], false)
	case "import":
		// First install of a language/country into this instance.
		return cmdLangForward(ctx, "import", args[1:], true)
	case "update":
		// Idempotent refresh of languages already on this instance with the
		// latest community strings. With no --country/--lang, cw-lang refreshes
		// every locale the instance currently manages.
		return cmdLangForward(ctx, "update", args[1:], true)
	case "list":
		// List catalogue languages; --installed annotates against this instance.
		return cmdLangForward(ctx, "list", args[1:], false)
	case "stats":
		// Local key-coverage of every language vs the English source.
		return cmdLangForward(ctx, "stats", args[1:], false)
	case "-h", "--help", "help":
		fmt.Print(`Usage: cockroach-cli lang <subcommand>

Subcommands:
  enable   Turn a language ON for this instance (upsert its ui_locales row).
           Talks DIRECTLY to $DATABASE_URL — no running server / token needed.
  disable  Turn a language OFF (enabled=FALSE). Strings are PRESERVED, so
           re-enabling is instant. Also direct-to-DB.
  import   Install UI translations for a country/language into this instance
  update   Refresh languages already installed here with the latest strings
           (idempotent; defaults to every locale this instance manages)
  list     List catalogue languages (--installed shows what THIS instance has)
  stats    Local per-language key-coverage vs the English source

All write paths are idempotent UPSERTs: re-running never deletes rows, never
touches English (baked-in), and never clobbers an operator's 'override' edits.
'en' is rejected by enable/disable — it is the baked-in fallback, not a managed
locale.

Environment:
  DATABASE_URL    Target Postgres DSN for enable/disable (default: dev cluster).
  LANG_SEED_DIR   Override the sibling cockroach-world-language checkout path
                  (default ../cockroach-world-language). enable reads its
                  languages.json for label/native_name/dir metadata.
  CW_INSTANCE     Default --instance base URL (forwarded to cw-lang).
  CW_ADMIN_TOKEN  Default --token admin bearer (forwarded to cw-lang).

Examples:
  cockroach-cli lang enable hi bn ta                       # turn on three locales
  cockroach-cli lang disable ur                            # hide one (strings kept)
  cockroach-cli lang import --country IN --instance https://your.org --token "$TOKEN"
  cockroach-cli lang update --country IN --instance https://your.org --token "$TOKEN"
  cockroach-cli lang update --instance https://your.org --token "$TOKEN"   # all installed
  cockroach-cli lang list   --installed --instance https://your.org --token "$TOKEN"
  cockroach-cli lang stats
  LANG_SEED_DIR=/path/to/cw-lang cockroach-cli lang update --country IN ...

Run "cockroach-cli lang <subcommand> --help" for flags.
`)
		return nil
	default:
		return UsageErrorf("unknown lang subcommand %q (try: enable, disable, import, update, list, stats)", args[0])
	}
}

// cmdLangSetEnabled enables or disables one or more locales by writing
// `ui_locales` rows directly against $DATABASE_URL — no running server, no admin
// token. This is the dev-reseed / CI path the admin HTTP API made awkward.
//
// Contract (mirrors the server's UITranslationsService):
//   - 'en' is rejected (baked-in fallback, never a managed locale).
//   - enable is an UPSERT that (re)populates label/native_name/dir from the
//     sibling languages.json, defaulting sensibly when the repo/entry is absent.
//   - disable is an UPSERT too, so disabling a locale the instance has never
//     enabled still records the intent (enabled=FALSE) rather than erroring;
//     it NEVER touches ui_translations, so the strings survive for a fast
//     re-enable.
//
// Idempotent: re-running with the same args is a no-op success.
func cmdLangSetEnabled(ctx context.Context, args []string, enabled bool) error {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	var codes []string
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Printf(`Usage: cockroach-cli lang %s <code> [<code>…]

Toggle a UI language's ui_locales row directly against $DATABASE_URL (no
running server or admin token required). %s sets enabled=%t.

enable populates label/native_name/dir from the sibling
cockroach-world-language/languages.json (located via $LANG_SEED_DIR); a missing
repo or entry falls back to label=<CODE>, native_name=<code>, dir=ltr with a
warning. disable preserves all ui_translations strings so re-enabling is
instant. 'en' is rejected — it is the baked-in fallback.

Examples:
  cockroach-cli lang enable hi bn ta
  cockroach-cli lang disable ur
`, verb, verb, enabled)
			return flag.ErrHelp
		}
		codes = append(codes, a)
	}
	if len(codes) == 0 {
		return UsageErrorf("usage: cockroach-cli lang %s <code> [<code>…]", verb)
	}

	// Validate every code up front so a bad arg can't leave a partial apply.
	// Reduce to the base code (strip any region/script subtag) so storage
	// matches the server's convention (ui_locales rows are keyed by base code,
	// e.g. "hi" not "hi-IN") and so "en-US" is caught by the 'en' guard below.
	normalised := make([]string, 0, len(codes))
	for _, c := range codes {
		code := baseLocale(c)
		if code == "" {
			return UsageErrorf("empty language code (usage: cockroach-cli lang %s <code> [<code>…])", verb)
		}
		if code == "en" {
			return UsageErrorf("'en' is the baked-in fallback and cannot be managed as a locale (it is always available; the API prepends English itself)")
		}
		normalised = append(normalised, code)
	}

	// Metadata source — best-effort: a missing sibling repo must NOT hard-fail
	// an enable, so we resolve once and fall back per-code below.
	var meta map[string]langMeta
	if enabled {
		meta = loadLangMeta()
	}

	pool, err := openPool(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	for _, code := range normalised {
		if enabled {
			m, ok := meta[code]
			if !ok {
				m = langMeta{Label: strings.ToUpper(code), NativeName: code, Dir: "ltr"}
				fmt.Fprintf(os.Stderr, "warning: %q not found in languages.json — enabling with fallback metadata (label=%s, native_name=%s, dir=ltr)\n", code, m.Label, m.NativeName)
			}
			if _, err := pool.Exec(ctx,
				`INSERT INTO ui_locales (locale, label, native_name, dir, enabled, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, TRUE, NOW(), NOW())
				 ON CONFLICT (locale) DO UPDATE
				   SET label       = EXCLUDED.label,
				       native_name = EXCLUDED.native_name,
				       dir         = EXCLUDED.dir,
				       enabled     = TRUE,
				       updated_at  = NOW()`,
				code, m.Label, m.NativeName, m.Dir); err != nil {
				return fmt.Errorf("enable %s: %w", code, err)
			}
			fmt.Printf("enabled %s (%s / %s, %s)\n", code, m.Label, m.NativeName, m.Dir)
			continue
		}
		// Disable: keep the row's metadata, only flip the flag. UPSERT so a
		// not-yet-seen code records the FALSE intent without metadata lookup;
		// ui_translations is deliberately untouched.
		if _, err := pool.Exec(ctx,
			`INSERT INTO ui_locales (locale, enabled, created_at, updated_at)
			 VALUES ($1, FALSE, NOW(), NOW())
			 ON CONFLICT (locale) DO UPDATE
			   SET enabled    = FALSE,
			       updated_at = NOW()`,
			code); err != nil {
			return fmt.Errorf("disable %s: %w", code, err)
		}
		fmt.Printf("disabled %s (strings preserved)\n", code)
	}
	return nil
}

// langMeta is the subset of a languages.json entry the ui_locales row needs.
type langMeta struct {
	Label      string // English-facing label (entry.english_name)
	NativeName string // endonym (entry.native_name)
	Dir        string // 'ltr' | 'rtl'
}

// loadLangMeta reads the sibling cockroach-world-language/languages.json and
// returns per-code metadata. Best-effort by design: a missing checkout or
// unparseable file yields an empty map (the caller falls back per-code) so the
// sibling repo's absence never blocks an enable on a fresh dev box.
func loadLangMeta() map[string]langMeta {
	out := map[string]langMeta{}
	dir, ok := resolveLangSeedDir()
	if !ok {
		fmt.Fprintf(os.Stderr, "warning: no cockroach-world-language checkout found — enabling with fallback metadata (set $LANG_SEED_DIR for native_name/dir)\n")
		return out
	}
	raw, err := os.ReadFile(filepath.Join(dir, "languages.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read %s/languages.json (%v) — enabling with fallback metadata\n", dir, err)
		return out
	}
	var doc struct {
		Languages map[string]struct {
			NativeName  string `json:"native_name"`
			EnglishName string `json:"english_name"`
			Dir         string `json:"dir"`
		} `json:"languages"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not parse %s/languages.json (%v) — enabling with fallback metadata\n", dir, err)
		return out
	}
	for code, e := range doc.Languages {
		label := e.EnglishName
		if label == "" {
			label = strings.ToUpper(code)
		}
		native := e.NativeName
		if native == "" {
			native = code
		}
		dir := strings.ToLower(strings.TrimSpace(e.Dir))
		if dir != "rtl" {
			dir = "ltr"
		}
		out[baseLocale(code)] = langMeta{Label: label, NativeName: native, Dir: dir}
	}
	return out
}

// cmdLangForward locates the sibling cockroach-world-language checkout and runs
// its `cw-lang <sub>` with the user's flags forwarded verbatim. When
// ownsCountryLang is true (import / update) it pre-extracts --country/--lang so
// it can normalise the country code before forwarding; the rest of the surface
// (--instance, --token, --apps, --dry-run, --installed, …) passes through
// untouched. For list / stats nothing is owned — every arg is forwarded as-is.
func cmdLangForward(ctx context.Context, sub string, args []string, ownsCountryLang bool) error {
	printUsage := func() {
		switch sub {
		case "list", "stats":
			fmt.Fprintf(os.Stderr, "Usage: cockroach-cli lang %s [cw-lang flags…]\n\n"+
				"Forwarded verbatim to the sibling cockroach-world-language `cw-lang %s`.\n"+
				"Run `cockroach-cli lang %s --help` for its flags.\n", sub, sub, sub)
		default:
			fmt.Fprintf(os.Stderr, `Usage: cockroach-cli lang %s [--country <cc> | --lang <code>] [cw-lang flags…]

Run the sibling cockroach-world-language checkout's `+"`cw-lang %s`"+` against this
instance's DB-driven i18n store. Located via $LANG_SEED_DIR, else
../cockroach-world-language; if absent, clone-and-run instructions are printed.

Flags beyond --country/--lang forward VERBATIM to cw-lang, e.g.:
  --instance <url>   target instance base URL
  --token <bearer>   admin bearer token (or env CW_ADMIN_TOKEN)
  --apps web,admin   limit to specific apps
  --dry-run          preview without sending
`, sub, sub)
		}
	}

	var country, lang string
	var rest []string
	var helpRequested bool
	var err error
	if ownsCountryLang {
		rest, helpRequested, err = extractRecognised(args, map[string]*string{
			"country": &country,
			"lang":    &lang,
		})
		if err != nil {
			printUsage()
			return err
		}
	} else {
		// list / stats own no flags — forward everything, but still honour -h.
		rest = args
		for _, a := range args {
			if a == "-h" || a == "--help" {
				helpRequested = true
			}
		}
	}
	if helpRequested {
		printUsage()
		return flag.ErrHelp
	}
	// `update` with neither flag is valid: cw-lang refreshes every installed
	// locale. `import` still requires a target so a typo can't no-op silently.
	if sub == "import" && country == "" && lang == "" {
		printUsage()
		return UsageErrorf("provide --country <cc> or --lang <code>")
	}

	dir, ok := resolveLangSeedDir()
	if !ok {
		printLangSeedGuidance(dir)
		return fmt.Errorf("no cockroach-world-language checkout found (clone it as shown above)")
	}

	runner, base := langRunner(dir)
	if runner == "" {
		return fmt.Errorf("cockroach-world-language at %s has no bin/cw-lang\n  Try: cd %s && pnpm install  (or set $LANG_SEED_DIR to a working checkout)", dir, dir)
	}

	// Build: <runner> <sub> [--country cc | --lang code] <forwarded flags>
	cmdArgs := append([]string{}, base...)
	cmdArgs = append(cmdArgs, sub)
	if country != "" {
		cc, err := normaliseCountry(country)
		if err != nil {
			return err
		}
		cmdArgs = append(cmdArgs, "--country", cc)
	}
	if lang != "" {
		cmdArgs = append(cmdArgs, "--lang", lang)
	}
	cmdArgs = append(cmdArgs, rest...) // forward the rest to cw-lang verbatim

	// list / stats are read-only; only the write paths announce the target dir.
	switch sub {
	case "import":
		fmt.Printf("==> importing translations via %s\n", dir)
	case "update":
		fmt.Printf("==> updating translations via %s\n", dir)
	}
	cmd := exec.CommandContext(ctx, runner, cmdArgs...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run cw-lang: %w", err)
	}
	return nil
}

// extractRecognised pulls out --key=value / --key value (and -key forms) for
// the keys named in `recognised`, writing them into the supplied destinations.
// Every other arg is returned verbatim in `rest` so the caller can forward
// them to a downstream tool (here: cw-lang). Returns helpRequested=true when
// --help / -h appears anywhere in args so the caller can short-circuit to the
// usage banner.
//
// Supported forms for a recognised key K:
//
//	--K=value       (empty value allowed → empty string is set)
//	--K value
//	-K=value
//	-K value
//
// A trailing recognised flag with no value (e.g. `--country` at end of args)
// is an error. A "--" terminator stops parsing — every subsequent arg, even
// if it looks like a recognised key, passes through to `rest` unchanged.
func extractRecognised(args []string, recognised map[string]*string) (rest []string, helpRequested bool, err error) {
	rest = make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		a := args[i]
		// `--` literal terminator: everything after is forwarded verbatim.
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			return rest, helpRequested, nil
		}
		// --help / -h short-circuit.
		if a == "--help" || a == "-h" || a == "-help" || a == "--h" {
			helpRequested = true
			i++
			continue
		}
		// Recognise --key / --key=val / -key / -key=val.
		key, val, hasVal, isFlag := parseFlagToken(a)
		if !isFlag {
			rest = append(rest, a)
			i++
			continue
		}
		dest, known := recognised[key]
		if !known {
			// Unknown flag — pass through verbatim to the downstream tool.
			rest = append(rest, a)
			i++
			continue
		}
		if hasVal {
			*dest = val
			i++
			continue
		}
		// Space-separated form: `--key value`.
		if i+1 >= len(args) {
			return rest, helpRequested, UsageErrorf("flag --%s requires a value", key)
		}
		*dest = args[i+1]
		i += 2
	}
	return rest, helpRequested, nil
}

// parseFlagToken inspects a single CLI token. Returns:
//
//	key       the flag name without leading dashes
//	val       the inline value when the token uses --key=val form
//	hasVal    true iff an '=' was present in the token
//	isFlag    true iff the token starts with one or two leading dashes
//	          AND has a non-empty key (so `--` alone returns isFlag=false)
//
// "-foo" and "--foo" are both treated as flags; downstream consumers (here
// cw-lang) can decide whether short vs long matters for THEIR options.
func parseFlagToken(tok string) (key, val string, hasVal, isFlag bool) {
	if len(tok) < 2 || tok[0] != '-' {
		return "", "", false, false
	}
	body := tok[1:]
	if len(body) > 0 && body[0] == '-' {
		body = body[1:]
	}
	if body == "" {
		return "", "", false, false
	}
	if eq := indexByte(body, '='); eq >= 0 {
		return body[:eq], body[eq+1:], true, true
	}
	return body, "", false, true
}

// indexByte is a tiny stdlib shim so this file stays self-contained — pulling
// in strings.IndexByte just for one call would expand the import list.
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// resolveLangSeedDir returns the cockroach-world-language checkout and whether
// it exists; on miss it returns the conventional clone target.
func resolveLangSeedDir() (string, bool) {
	if v := os.Getenv("LANG_SEED_DIR"); v != "" {
		return v, dirExists(v)
	}
	conventional := filepath.Join("..", "cockroach-world-language")
	if dirExists(conventional) {
		return conventional, true
	}
	fromBackend := filepath.Join("..", "..", "cockroach-world-language")
	if dirExists(fromBackend) {
		return fromBackend, true
	}
	return conventional, false
}

// langRunner picks how to invoke cw-lang. The repo ships a python entry point
// (bin/cw-lang) plus a bash wrapper (bin/cw-lang.sh). We run the python tool
// directly via the interpreter for portability (Windows has no shebang).
func langRunner(dir string) (string, []string) {
	py := filepath.Join("bin", "cw-lang")
	if fileExists(filepath.Join(dir, py)) {
		interp := "python3"
		if runtime.GOOS == "windows" {
			interp = "python"
		}
		return interp, []string{py}
	}
	return "", nil
}

func printLangSeedGuidance(conventional string) {
	fmt.Fprintf(os.Stderr, `No cockroach-world-language checkout found.

UI translations are community-maintained in their own repo and pushed into a
running instance via the admin import API. Clone it as a sibling, then re-run:

  git clone https://github.com/cockroachnetworkorg/cockroach-world-language %s
  cockroach-cli lang import --country IN --instance https://your-instance.org --token "$ADMIN_TOKEN"

Or point LANG_SEED_DIR at an existing checkout:

  LANG_SEED_DIR=/path/to/cockroach-world-language cockroach-cli lang import --country IN ...

The import needs an admin bearer token and your instance base URL (passed
through to cw-lang as --token / --instance).
`, conventional)
}
