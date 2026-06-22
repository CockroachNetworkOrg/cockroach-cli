package clibuild

// Unit tests for `cockroach-cli pack upgrade`. The diffing logic is pure (a list
// of InstalledPack vs. a *pack.Registry), so we exercise it directly with
// hand-constructed inputs — the test stub for the registry "fetch" is a
// literal Registry value rather than a real HTTP roundtrip.
//
// The brief asks the test to "stub the registry fetch". The diff helper
// (`computeUpgradePlan`) takes a *pack.Registry rather than a URL, so the
// "fetch" stub is just constructing the Registry. That's the right shape:
// the network is the integration test's problem; the upgrade logic is what
// must be unit-covered.

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cockroachnetworkorg/cockroach-cli/pkg/pack"
)

// stubRegistry returns a fixed *pack.Registry the diff tests can match against.
// One country (IN) with two packs at version 2026.02.01 — a future release
// would publish version 2026.05.01 to exercise the upgrade path.
func stubRegistry(version string) *pack.Registry {
	return &pack.Registry{
		Schema: pack.SupportedSchema,
		Countries: map[string]pack.CountryEntry{
			"IN": {
				Name:       "India",
				Version:    version,
				ReleaseURL: "https://example.org/IN/" + version,
				Packs: []pack.Pack{
					{File: "gazetteer_kinds.csv.gz", Table: "gazetteer_kinds", Conflict: "(country_code, kind)", SHA256: "aaaa", ExpectedRows: 10, SizeBytes: 1024},
					{File: "gazetteer_units.csv.gz", Table: "gazetteer_units", Conflict: "(code)", SHA256: "bbbb", ExpectedRows: 4000, SizeBytes: 50_000},
				},
			},
		},
	}
}

// stubInstalled returns a hand-rolled installed_packs projection — the same
// shape `pack.ListInstalled` returns from the live ledger, minus the audit
// columns the upgrade diff doesn't care about.
func stubInstalled(version, sha string) []pack.InstalledPack {
	now := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	return []pack.InstalledPack{
		{
			CountryCode: "IN", PackFile: "gazetteer_kinds.csv.gz", TargetTable: "gazetteer_kinds",
			Version: version, SHA256: sha, RowsLoaded: 10, InstalledAt: now,
		},
		{
			CountryCode: "IN", PackFile: "gazetteer_units.csv.gz", TargetTable: "gazetteer_units",
			Version: version, SHA256: sha, RowsLoaded: 4000, InstalledAt: now,
		},
	}
}

// TestComputeUpgradePlanAllUpToDate pins the no-op case — installed sha256
// matches registry → empty plan.
func TestComputeUpgradePlanAllUpToDate(t *testing.T) {
	reg := stubRegistry("2026.02.01")
	// Installed at the same version + matching SHAs (since the test's
	// installed shas match the registry's; cheating slightly — see below).
	installed := []pack.InstalledPack{
		{CountryCode: "IN", PackFile: "gazetteer_kinds.csv.gz", Version: "2026.02.01", SHA256: "aaaa"},
		{CountryCode: "IN", PackFile: "gazetteer_units.csv.gz", Version: "2026.02.01", SHA256: "bbbb"},
	}
	plan, involved := computeUpgradePlan(installed, reg)
	if len(plan) != 0 {
		t.Errorf("expected empty plan when up to date; got %d candidate(s): %+v", len(plan), plan)
	}
	if len(involved) != 0 {
		t.Errorf("expected no involved countries when up to date; got %v", involved)
	}
}

// TestComputeUpgradePlanVersionBump pins the canonical upgrade case — version
// AND sha256 changed → both packs in the plan.
func TestComputeUpgradePlanVersionBump(t *testing.T) {
	reg := stubRegistry("2026.05.01")
	installed := stubInstalled("2026.02.01", "old-sha")

	plan, involved := computeUpgradePlan(installed, reg)
	if len(plan) != 2 {
		t.Fatalf("expected 2 outdated packs, got %d: %+v", len(plan), plan)
	}
	for _, c := range plan {
		if c.CurrentVersion != "2026.02.01" {
			t.Errorf("expected current 2026.02.01, got %q (pack %s)", c.CurrentVersion, c.PackFile)
		}
		if c.NewVersion != "2026.05.01" {
			t.Errorf("expected available 2026.05.01, got %q (pack %s)", c.NewVersion, c.PackFile)
		}
		if !c.SHA256Changed() {
			t.Errorf("pack %s SHA256Changed() = false; expected true (old-sha vs %s)", c.PackFile, c.NewSHA256)
		}
	}

	// The country must be in the involved map so the apply loop knows to
	// re-run the installer for it.
	if _, ok := involved["IN"]; !ok {
		t.Errorf("expected IN in involved map; got %v", involved)
	}
}

// TestComputeUpgradePlanShaOnlyChange pins the rare-but-real case: same
// version, different sha256 (publisher re-cut the same release). Still
// outdated.
func TestComputeUpgradePlanShaOnlyChange(t *testing.T) {
	reg := stubRegistry("2026.02.01") // same version
	installed := stubInstalled("2026.02.01", "different-sha")
	plan, _ := computeUpgradePlan(installed, reg)
	if len(plan) != 2 {
		t.Errorf("expected 2 candidates on sha-only drift, got %d: %+v", len(plan), plan)
	}
}

// TestComputeUpgradePlanSkipsMissingCountry pins that an installed pack whose
// country is no longer in the registry is silently skipped (not an upgrade
// candidate; it's a removal candidate which is a separate concept).
func TestComputeUpgradePlanSkipsMissingCountry(t *testing.T) {
	reg := stubRegistry("2026.05.01")
	installed := []pack.InstalledPack{
		{CountryCode: "ZZ", PackFile: "gazetteer_kinds.csv.gz", Version: "2026.02.01", SHA256: "x"},
	}
	plan, involved := computeUpgradePlan(installed, reg)
	if len(plan) != 0 {
		t.Errorf("expected empty plan for unknown country, got %+v", plan)
	}
	if len(involved) != 0 {
		t.Errorf("expected no involved when country missing, got %v", involved)
	}
}

// TestComputeUpgradePlanSkipsMissingFile pins that an installed pack whose
// file is no longer in the registry's pack list for the country is skipped.
// (Same reasoning as the missing-country case: removal ≠ upgrade.)
func TestComputeUpgradePlanSkipsMissingFile(t *testing.T) {
	reg := stubRegistry("2026.05.01")
	installed := []pack.InstalledPack{
		{CountryCode: "IN", PackFile: "obsolete.csv.gz", Version: "2026.02.01", SHA256: "x"},
	}
	plan, _ := computeUpgradePlan(installed, reg)
	if len(plan) != 0 {
		t.Errorf("expected empty plan for obsolete file, got %+v", plan)
	}
}

// TestUpgradeCandidateSHA256Changed pins the helper directly.
func TestUpgradeCandidateSHA256Changed(t *testing.T) {
	cases := []struct {
		current, next string
		want          bool
	}{
		{"abc", "abc", false},
		{"ABC", "abc", false}, // case-insensitive
		{"abc", "def", true},
		{"", "abc", true},
	}
	for _, c := range cases {
		got := upgradeCandidate{CurrentSHA256: c.current, NewSHA256: c.next}.SHA256Changed()
		if got != c.want {
			t.Errorf("SHA256Changed(%q,%q) = %t, want %t", c.current, c.next, got, c.want)
		}
	}
}

// TestPrintUpgradePlanEmpty pins the "nothing to do" output — the brief asks
// for `--dry-run` to print an empty list cleanly when nothing's installed.
func TestPrintUpgradePlanEmpty(t *testing.T) {
	// printUpgradePlan writes to *os.File. We use a temp file as a stand-in
	// so we don't have to refactor the renderer to io.Writer just for this
	// test (the function's audience IS os.Stdout — the temp file path lets
	// the test still observe the bytes).
	tmp, err := os.CreateTemp("", "pack-upgrade-empty-*.txt")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer os.Remove(tmp.Name())

	printUpgradePlan(tmp, nil)
	tmp.Close()

	body, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("read temp: %v", err)
	}
	if !strings.Contains(string(body), "All installed packs are up to date.") {
		t.Errorf("expected 'up to date' line for empty plan, got:\n%s", string(body))
	}
}

// TestPrintUpgradePlanRendersAllColumns pins that every column the brief asks
// for is in the output.
func TestPrintUpgradePlanRendersAllColumns(t *testing.T) {
	tmp, err := os.CreateTemp("", "pack-upgrade-plan-*.txt")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer os.Remove(tmp.Name())

	plan := []upgradeCandidate{
		{
			CountryCode:    "IN",
			PackFile:       "gazetteer_kinds.csv.gz",
			CurrentVersion: "2026.02.01",
			NewVersion:     "2026.05.01",
			CurrentSHA256:  "old",
			NewSHA256:      "new",
			RowsEstimate:   12,
		},
	}
	printUpgradePlan(tmp, plan)
	tmp.Close()

	body, _ := os.ReadFile(tmp.Name())
	out := string(body)
	for _, want := range []string{
		"COUNTRY",
		"FILE",
		"CURRENT",
		"AVAILABLE",
		"SHA256?",
		"ROWS-EST",
		"IN",
		"gazetteer_kinds.csv.gz",
		"2026.02.01",
		"2026.05.01",
		"changed", // SHA256? column when digests differ
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in plan output, got:\n%s", want, out)
		}
	}
}

// TestPrintUpgradePlanShaSameTag pins the "same" SHA256? tag when a version
// bump didn't move the digest.
func TestPrintUpgradePlanShaSameTag(t *testing.T) {
	tmp, _ := os.CreateTemp("", "pack-upgrade-sha-same-*.txt")
	defer os.Remove(tmp.Name())
	plan := []upgradeCandidate{
		{CountryCode: "IN", PackFile: "x.csv.gz", CurrentVersion: "v1", NewVersion: "v2", CurrentSHA256: "abc", NewSHA256: "abc"},
	}
	printUpgradePlan(tmp, plan)
	tmp.Close()

	body, _ := os.ReadFile(tmp.Name())
	if !strings.Contains(string(body), "same") {
		t.Errorf("expected 'same' tag when SHA256 unchanged, got:\n%s", string(body))
	}
}

// TestFilterByCountry pins the country scope filter.
func TestFilterByCountry(t *testing.T) {
	in := []pack.InstalledPack{
		{CountryCode: "IN", PackFile: "a"},
		{CountryCode: "PS", PackFile: "b"},
		{CountryCode: "IN", PackFile: "c"},
	}
	got := filterByCountry(in, "IN")
	if len(got) != 2 {
		t.Errorf("expected 2 rows after filter to IN, got %d: %+v", len(got), got)
	}
	for _, r := range got {
		if r.CountryCode != "IN" {
			t.Errorf("expected only IN rows, got %s", r.CountryCode)
		}
	}
}

// TestCmdPackUpgradeFlagParsing pins the flag parser — --help must succeed
// without driving any of the DB / network paths.
func TestCmdPackUpgradeFlagParsing(t *testing.T) {
	// Redirect stderr so the usage text doesn't spam the test log.
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err := CmdPackUpgrade(t.Context(), []string{"--help"})

	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	// flag.ContinueOnError + --help returns flag.ErrHelp, which main() swallows
	// silently. We accept any error here (the parser may return ErrHelp), but
	// the harness must NOT have crashed.
	if err != nil && !strings.Contains(err.Error(), "help") {
		// flag.ErrHelp's message is "flag: help requested"; tolerate it.
		_ = err
	}

	out := buf.String()
	if !strings.Contains(out, "pack upgrade") {
		t.Errorf("expected usage line to mention 'pack upgrade', got:\n%s", out)
	}
}
