package clibuild

// Unit tests for the `cockroach-cli lang import` flag-forwarding helper. The
// command's job is to pull out --country / --lang and forward EVERYTHING else
// verbatim to cw-lang — see L1 in the cli-fixes patch. These tests pin the
// extractRecognised contract: every --key=val / --key val form is recognised,
// every UNKNOWN flag passes through to `rest`, --help short-circuits without
// requiring values, and a trailing recognised flag with no value errors.

import (
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
)

// TestExtractRecognisedKeyEqValue pins `--country=IN`.
func TestExtractRecognisedKeyEqValue(t *testing.T) {
	var country, lang string
	rest, help, err := extractRecognised([]string{"--country=IN"}, map[string]*string{
		"country": &country, "lang": &lang,
	})
	if err != nil {
		t.Fatalf("--country=IN: %v", err)
	}
	if help {
		t.Errorf("--country=IN must not request help")
	}
	if country != "IN" {
		t.Errorf("expected country=IN, got %q", country)
	}
	if lang != "" {
		t.Errorf("expected empty lang, got %q", lang)
	}
	if len(rest) != 0 {
		t.Errorf("expected empty rest, got %v", rest)
	}
}

// TestExtractRecognisedKeySpaceValue pins `--country IN` (space form).
func TestExtractRecognisedKeySpaceValue(t *testing.T) {
	var country, lang string
	rest, _, err := extractRecognised([]string{"--country", "IN"}, map[string]*string{
		"country": &country, "lang": &lang,
	})
	if err != nil {
		t.Fatalf("--country IN: %v", err)
	}
	if country != "IN" {
		t.Errorf("expected country=IN, got %q", country)
	}
	if len(rest) != 0 {
		t.Errorf("expected empty rest, got %v", rest)
	}
}

// TestExtractRecognisedShortDashForm pins the single-dash form `-country in`
// — cw-lang accepts both and Go's flag stdlib treats them identically; the
// helper must too.
func TestExtractRecognisedShortDashForm(t *testing.T) {
	var country, lang string
	_, _, err := extractRecognised([]string{"-country", "in"}, map[string]*string{
		"country": &country, "lang": &lang,
	})
	if err != nil {
		t.Fatalf("-country in: %v", err)
	}
	if country != "in" {
		t.Errorf("expected country=in (case preserved by helper), got %q", country)
	}
}

// TestExtractRecognisedEmptyValueAllowed pins the `--country=` (empty value)
// edge case — the helper SETS the dest to "" so the caller's required-flag
// check still fires the right error message, instead of looking like the flag
// was absent.
func TestExtractRecognisedEmptyValueAllowed(t *testing.T) {
	var country, lang string
	rest, _, err := extractRecognised([]string{"--country="}, map[string]*string{
		"country": &country, "lang": &lang,
	})
	if err != nil {
		t.Fatalf("--country=: %v", err)
	}
	// Empty-value dest write IS still a write — confirm rest stays empty,
	// confirming the flag was consumed (not passed through).
	if len(rest) != 0 {
		t.Errorf("--country= must be consumed, not forwarded; rest=%v", rest)
	}
	if country != "" {
		t.Errorf("expected empty country, got %q", country)
	}
}

// TestExtractRecognisedMissingValueErrors pins the failure mode — a recognised
// flag at end-of-args with no following value is a usage error.
func TestExtractRecognisedMissingValueErrors(t *testing.T) {
	var country, lang string
	_, _, err := extractRecognised([]string{"--country"}, map[string]*string{
		"country": &country, "lang": &lang,
	})
	if err == nil {
		t.Fatal("expected error for --country without a value")
	}
	if !strings.Contains(err.Error(), "requires a value") {
		t.Errorf("expected 'requires a value' in error, got: %v", err)
	}
}

// TestExtractRecognisedUnknownFlagsPassThrough pins the core forwarding
// behaviour — every flag NOT in the recognised map (and every positional)
// flows through to `rest` so cw-lang sees them verbatim.
func TestExtractRecognisedUnknownFlagsPassThrough(t *testing.T) {
	var country, lang string
	rest, _, err := extractRecognised([]string{
		"--country", "IN",
		"--instance", "http://localhost:13001",
		"--token", "TOK",
		"--apps", "web,admin",
		"--dry-run",
	}, map[string]*string{
		"country": &country, "lang": &lang,
	})
	if err != nil {
		t.Fatalf("mixed flags: %v", err)
	}
	if country != "IN" {
		t.Errorf("expected country=IN, got %q", country)
	}
	want := []string{
		"--instance", "http://localhost:13001",
		"--token", "TOK",
		"--apps", "web,admin",
		"--dry-run",
	}
	if len(rest) != len(want) {
		t.Fatalf("expected %d forwarded args, got %d: %v", len(want), len(rest), rest)
	}
	for i := range want {
		if rest[i] != want[i] {
			t.Errorf("rest[%d] = %q, want %q (full: %v)", i, rest[i], want[i], rest)
		}
	}
}

// TestExtractRecognisedHelpShortCircuits pins that --help / -h returns
// helpRequested=true so the caller can render its own usage banner without
// trying to validate the missing required flags.
func TestExtractRecognisedHelpShortCircuits(t *testing.T) {
	for _, h := range []string{"--help", "-h"} {
		t.Run(h, func(t *testing.T) {
			var country, lang string
			_, help, err := extractRecognised([]string{h}, map[string]*string{
				"country": &country, "lang": &lang,
			})
			if err != nil {
				t.Fatalf("%s: %v", h, err)
			}
			if !help {
				t.Errorf("%s must set helpRequested=true", h)
			}
		})
	}
}

// TestExtractRecognisedDoubleDashTerminator pins the POSIX `--` terminator —
// every arg after it is forwarded verbatim even if it would otherwise have
// been recognised. Lets operators forward an option that COLLIDES with a
// recognised key (rare but worth supporting).
func TestExtractRecognisedDoubleDashTerminator(t *testing.T) {
	var country, lang string
	rest, _, err := extractRecognised([]string{
		"--country", "IN",
		"--",
		"--lang", "hi", // after `--` so it is forwarded, NOT consumed
	}, map[string]*string{
		"country": &country, "lang": &lang,
	})
	if err != nil {
		t.Fatalf("terminator: %v", err)
	}
	if country != "IN" {
		t.Errorf("expected country=IN, got %q", country)
	}
	if lang != "" {
		t.Errorf("--lang after `--` must NOT be consumed; got lang=%q", lang)
	}
	wantRest := []string{"--lang", "hi"}
	if len(rest) != len(wantRest) {
		t.Fatalf("expected %v in rest, got %v", wantRest, rest)
	}
	for i := range wantRest {
		if rest[i] != wantRest[i] {
			t.Errorf("rest[%d] = %q, want %q", i, rest[i], wantRest[i])
		}
	}
}

// TestExtractRecognisedPositionalsPreserved pins that bare positionals (no
// leading dash) flow through to `rest` in input order.
func TestExtractRecognisedPositionalsPreserved(t *testing.T) {
	var country, lang string
	rest, _, err := extractRecognised([]string{
		"positional-one", "--country", "IN", "positional-two",
	}, map[string]*string{
		"country": &country, "lang": &lang,
	})
	if err != nil {
		t.Fatalf("positionals: %v", err)
	}
	if country != "IN" {
		t.Errorf("expected country=IN, got %q", country)
	}
	wantRest := []string{"positional-one", "positional-two"}
	if len(rest) != len(wantRest) {
		t.Fatalf("expected %v in rest, got %v", wantRest, rest)
	}
	for i := range wantRest {
		if rest[i] != wantRest[i] {
			t.Errorf("rest[%d] = %q, want %q", i, rest[i], wantRest[i])
		}
	}
}

// TestExtractRecognisedBothKeysSet pins that --country AND --lang can BOTH be
// set in the same invocation — the caller's `country==""` / `lang==""` checks
// decide which path runs.
func TestExtractRecognisedBothKeysSet(t *testing.T) {
	var country, lang string
	_, _, err := extractRecognised([]string{
		"--country", "IN", "--lang=hi",
	}, map[string]*string{
		"country": &country, "lang": &lang,
	})
	if err != nil {
		t.Fatalf("both keys: %v", err)
	}
	if country != "IN" || lang != "hi" {
		t.Errorf("expected country=IN, lang=hi; got %q / %q", country, lang)
	}
}

// TestParseFlagTokenForms pins the low-level parser the helper rides on so
// the boundary cases are nailed down with a focused test.
func TestParseFlagTokenForms(t *testing.T) {
	cases := []struct {
		in       string
		key, val string
		hasVal   bool
		isFlag   bool
	}{
		{"--country=IN", "country", "IN", true, true},
		{"--country", "country", "", false, true},
		{"-country", "country", "", false, true},
		{"-c=IN", "c", "IN", true, true},
		{"--", "", "", false, false},
		{"", "", "", false, false},
		{"positional", "", "", false, false},
		{"-", "", "", false, false},
		{"--country=", "country", "", true, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			k, v, hv, fl := parseFlagToken(c.in)
			if k != c.key || v != c.val || hv != c.hasVal || fl != c.isFlag {
				t.Errorf("parseFlagToken(%q) = (%q,%q,%v,%v); want (%q,%q,%v,%v)",
					c.in, k, v, hv, fl, c.key, c.val, c.hasVal, c.isFlag)
			}
		})
	}
}

// ── lang enable / disable arg parsing ───────────────────────────────────────
//
// These pin the validation that runs BEFORE any DB connection — no code → usage
// error, 'en' → rejected, -h → ErrHelp. All three short-circuit ahead of
// openPool, so the tests never touch a database (matching the no-DB style of
// the helper tests above).

// TestLangSetEnabledNoCodeIsUsageError pins that `enable`/`disable` with no
// code is a usage error (exit 2), not a silent no-op.
func TestLangSetEnabledNoCodeIsUsageError(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		err := cmdLangSetEnabled(context.Background(), nil, enabled)
		if err == nil {
			t.Fatalf("enabled=%v: expected usage error for missing code", enabled)
		}
		if !errors.Is(err, ErrUsage) {
			t.Errorf("enabled=%v: expected ErrUsage, got %v", enabled, err)
		}
	}
}

// TestLangSetEnabledRejectsEnglish pins that 'en' (any case / region subtag) is
// rejected before a DB connection — mirrors the server's ErrLocaleIsEnglish.
func TestLangSetEnabledRejectsEnglish(t *testing.T) {
	for _, code := range []string{"en", "EN", "en-US", " en "} {
		err := cmdLangSetEnabled(context.Background(), []string{code}, true)
		if err == nil {
			t.Fatalf("%q: expected rejection of English", code)
		}
		if !errors.Is(err, ErrUsage) {
			t.Errorf("%q: expected ErrUsage, got %v", code, err)
		}
		if !strings.Contains(err.Error(), "baked-in fallback") {
			t.Errorf("%q: expected 'baked-in fallback' in error, got: %v", code, err)
		}
	}
}

// TestLangSetEnabledHelpShortCircuits pins that -h / --help return
// flag.ErrHelp (so main() exits 0 after the flagset-style usage print) without
// attempting a DB connection.
func TestLangSetEnabledHelpShortCircuits(t *testing.T) {
	for _, h := range []string{"-h", "--help"} {
		err := cmdLangSetEnabled(context.Background(), []string{h}, true)
		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("%s: expected flag.ErrHelp, got %v", h, err)
		}
	}
}
