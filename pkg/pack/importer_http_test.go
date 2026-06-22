package pack

// Tests for the HTTP source path on InstallPack — i.e. `pack install --country`
// with NO --from-dir, the registry→download→verify path. The --from-dir tests
// cover the local source; these cover the network half: the URL is built from
// the entry's release_url, fetched, and run through the IDENTICAL sha256/size
// verification. DB-free via dry-run (download → verify → gunzip → row-count, then
// stop before COPY), exactly like the from-dir tests.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveGzipPack returns a test server that serves `body` at exactly "/<file>"
// (404 for anything else) and records the last requested path, so a test can
// assert the URL was built as release_url + "/" + file.
func serveGzipPack(t *testing.T, file string, body []byte) (*httptest.Server, *string) {
	t.Helper()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path != "/"+file {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body) // raw bytes; no Content-Encoding, so the client won't transparently gunzip
	}))
	t.Cleanup(srv.Close)
	return srv, &gotPath
}

// TestInstallPackHTTPHappyPath: a pack fetched over HTTP whose bytes match the
// registry's pinned sha256 + size + expected_rows verifies clean in dry-run, and
// the request URL is built from the entry's release_url.
func TestInstallPackHTTPHappyPath(t *testing.T) {
	gzBytes, sum := makeGzipCSV(t, "code,name\nIN-DL,Delhi\nIN-MH,Maharashtra\n")
	p := Pack{
		File:         "gazetteer_units.csv.gz",
		Table:        "gazetteer_units",
		Conflict:     "(code)",
		SHA256:       sum,
		SizeBytes:    int64(len(gzBytes)),
		ExpectedRows: 2,
	}
	srv, gotPath := serveGzipPack(t, p.File, gzBytes)

	im := &Installer{Client: srv.Client()} // no DB pool needed in dry-run
	entry := CountryEntry{Name: "India", Version: "2026.06.08", ReleaseURL: srv.URL, Packs: []Pack{p}}
	res, err := im.InstallPack(context.Background(), "IN", entry.Version, p,
		InstallOptions{DryRun: true}, entry)
	if err != nil {
		t.Fatalf("InstallPack(http) happy path: %v", err)
	}
	if res.BytesFetched != int64(len(gzBytes)) {
		t.Errorf("BytesFetched = %d, want %d", res.BytesFetched, len(gzBytes))
	}
	if res.RowsInCSV != 2 {
		t.Errorf("RowsInCSV = %d, want 2", res.RowsInCSV)
	}
	if *gotPath != "/"+p.File {
		t.Errorf("requested path = %q, want %q (DownloadURL = release_url + file)", *gotPath, "/"+p.File)
	}
}

// TestInstallPackHTTPSHA256Mismatch: bytes that don't match the registry's pinned
// digest abort loudly — the registry stays the source of truth over HTTP too.
func TestInstallPackHTTPSHA256Mismatch(t *testing.T) {
	gzBytes, _ := makeGzipCSV(t, "code,name\nIN-DL,Delhi\n")
	p := Pack{
		File:      "gazetteer_units.csv.gz",
		Table:     "gazetteer_units",
		Conflict:  "(code)",
		SHA256:    strings.Repeat("0", 64), // deliberately wrong digest
		SizeBytes: int64(len(gzBytes)),
	}
	srv, _ := serveGzipPack(t, p.File, gzBytes)

	im := &Installer{Client: srv.Client()}
	entry := CountryEntry{Name: "India", Version: "2026.06.08", ReleaseURL: srv.URL, Packs: []Pack{p}}
	_, err := im.InstallPack(context.Background(), "IN", entry.Version, p,
		InstallOptions{DryRun: true}, entry)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch error, got %v", err)
	}
}

// TestInstallPackHTTPNotFound: a missing release asset surfaces the HTTP status,
// not a corrupt-pack error.
func TestInstallPackHTTPNotFound(t *testing.T) {
	gzBytes, sum := makeGzipCSV(t, "code,name\nIN-DL,Delhi\n")
	p := Pack{
		File:      "gazetteer_units.csv.gz",
		Table:     "gazetteer_units",
		Conflict:  "(code)",
		SHA256:    sum,
		SizeBytes: int64(len(gzBytes)),
	}
	srv, _ := serveGzipPack(t, "other.csv.gz", gzBytes) // served under a different name → request 404s

	im := &Installer{Client: srv.Client()}
	entry := CountryEntry{Name: "India", Version: "2026.06.08", ReleaseURL: srv.URL, Packs: []Pack{p}}
	_, err := im.InstallPack(context.Background(), "IN", entry.Version, p,
		InstallOptions{DryRun: true}, entry)
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("expected HTTP 404 error, got %v", err)
	}
}
