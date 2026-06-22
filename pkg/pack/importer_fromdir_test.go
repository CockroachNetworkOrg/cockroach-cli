package pack

// Tests for the `--from-dir` local-pack source path on InstallPack. These stay
// DB-free by running in --dry-run mode: read the local file → sha256/size verify
// against the registry entry → gunzip → row-count, then stop before any COPY.
// They mirror the registry's verification contract — the local bytes must match
// the registry's pinned digest/size or the install aborts loudly.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeGzipCSV gzips a CSV body and returns the compressed bytes + their sha256.
func makeGzipCSV(t *testing.T, csv string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(csv)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	raw := buf.Bytes()
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

// TestInstallPackFromDirHappyPath: a local pack whose bytes match the registry's
// pinned sha256 + size + expected_rows verifies clean in dry-run (no DB).
func TestInstallPackFromDirHappyPath(t *testing.T) {
	dir := t.TempDir()
	csv := "code,name\nIN-DL,Delhi\nIN-MH,Maharashtra\n"
	gzBytes, sum := makeGzipCSV(t, csv)

	p := Pack{
		File:         "gazetteer_units.csv.gz",
		Table:        "gazetteer_units",
		Conflict:     "(code)",
		SHA256:       sum,
		SizeBytes:    int64(len(gzBytes)),
		ExpectedRows: 2,
	}
	if err := os.WriteFile(filepath.Join(dir, p.File), gzBytes, 0o644); err != nil {
		t.Fatalf("write pack: %v", err)
	}

	im := &Installer{} // no DB pool needed in dry-run
	entry := CountryEntry{Name: "India", Version: "2026.06.08", Packs: []Pack{p}}
	res, err := im.InstallPack(context.Background(), "IN", entry.Version, p,
		InstallOptions{DryRun: true, FromDir: dir}, entry)
	if err != nil {
		t.Fatalf("InstallPack(--from-dir) happy path: %v", err)
	}
	if res.BytesFetched != int64(len(gzBytes)) {
		t.Errorf("BytesFetched = %d, want %d", res.BytesFetched, len(gzBytes))
	}
	if res.RowsInCSV != 2 {
		t.Errorf("RowsInCSV = %d, want 2", res.RowsInCSV)
	}
}

// TestInstallPackFromDirSHA256Mismatch: a local pack whose bytes DON'T match the
// registry's pinned sha256 must abort loudly (the registry stays the source of
// truth even for local installs).
func TestInstallPackFromDirSHA256Mismatch(t *testing.T) {
	dir := t.TempDir()
	gzBytes, _ := makeGzipCSV(t, "code,name\nIN-DL,Delhi\n")

	p := Pack{
		File:      "gazetteer_units.csv.gz",
		Table:     "gazetteer_units",
		Conflict:  "(code)",
		SHA256:    strings.Repeat("0", 64), // deliberately wrong digest
		SizeBytes: int64(len(gzBytes)),
	}
	if err := os.WriteFile(filepath.Join(dir, p.File), gzBytes, 0o644); err != nil {
		t.Fatalf("write pack: %v", err)
	}

	im := &Installer{}
	entry := CountryEntry{Name: "India", Version: "2026.06.08", Packs: []Pack{p}}
	_, err := im.InstallPack(context.Background(), "IN", entry.Version, p,
		InstallOptions{DryRun: true, FromDir: dir}, entry)
	if err == nil {
		t.Fatal("expected sha256 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("expected sha256 mismatch error, got %v", err)
	}
}

// TestInstallPackFromDirSizeMismatch: a size that disagrees with the registry's
// pin aborts before hashing.
func TestInstallPackFromDirSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	gzBytes, sum := makeGzipCSV(t, "code,name\nIN-DL,Delhi\n")

	p := Pack{
		File:      "gazetteer_units.csv.gz",
		Table:     "gazetteer_units",
		Conflict:  "(code)",
		SHA256:    sum,
		SizeBytes: int64(len(gzBytes)) + 1, // off-by-one
	}
	if err := os.WriteFile(filepath.Join(dir, p.File), gzBytes, 0o644); err != nil {
		t.Fatalf("write pack: %v", err)
	}

	im := &Installer{}
	entry := CountryEntry{Name: "India", Version: "2026.06.08", Packs: []Pack{p}}
	_, err := im.InstallPack(context.Background(), "IN", entry.Version, p,
		InstallOptions{DryRun: true, FromDir: dir}, entry)
	if err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("expected size mismatch error, got %v", err)
	}
}

// TestInstallPackFromDirMissingFile: a FromDir that lacks the pack file surfaces
// a clear read error (no panic).
func TestInstallPackFromDirMissingFile(t *testing.T) {
	dir := t.TempDir()
	p := Pack{File: "absent.csv.gz", Table: "gazetteer_units", Conflict: "(code)", SHA256: strings.Repeat("a", 64)}
	im := &Installer{}
	entry := CountryEntry{Name: "India", Version: "2026.06.08", Packs: []Pack{p}}
	_, err := im.InstallPack(context.Background(), "IN", entry.Version, p,
		InstallOptions{DryRun: true, FromDir: dir}, entry)
	if err == nil || !strings.Contains(err.Error(), "read local pack") {
		t.Fatalf("expected read local pack error, got %v", err)
	}
}
