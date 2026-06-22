package clibuild

// Helpers shared by the core CLI commands: ISO country-code normalisation,
// locale canonicalisation, and trivial filesystem probes.

import (
	"os"
	"strings"
)

// normaliseCountry validates and upper-cases a 2-letter ISO 3166-1 alpha-2 code.
// Returns a usage error (exit 2) on a bad input so the operator sees the right
// exit code from CI.
func normaliseCountry(cc string) (string, error) {
	cc = strings.ToUpper(strings.TrimSpace(cc))
	if len(cc) != 2 || !isAlpha(cc) {
		return "", UsageErrorf("country code must be 2 letters (ISO 3166-1 alpha-2), got %q\n  Try: cockroach-cli pack list  # to see published countries", cc)
	}
	return cc, nil
}

// normaliseLang lower-cases and trims a language code (no validation — the
// registry schema polices the shape; this just canonicalises for lookup).
func normaliseLang(code string) string {
	return strings.ToLower(strings.TrimSpace(code))
}

// baseLocale reduces a locale to its base code: lower-cases, trims, and strips
// any region/script subtag ("hi-IN" → "hi", "EN" → "en"). Mirrors the server's
// service.normalizeLocale so the CLI stores ui_locales rows by the same key the
// API + frontend use, and so "en-US" is caught by an 'en' guard.
func baseLocale(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.IndexAny(s, "-_"); i >= 0 {
		s = s[:i]
	}
	return s
}

func isAlpha(s string) bool {
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
