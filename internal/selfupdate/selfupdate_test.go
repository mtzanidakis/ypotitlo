package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRelease serves a GitHub-shaped release with real archives, so the whole
// flow runs end to end without touching the network or the real executable.
type fakeRelease struct {
	tag     string
	assets  map[string][]byte // name -> archive bytes
	omitSum bool
	badSum  bool
}

func (f *fakeRelease) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/repos/owner/name/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		fmt.Fprintf(&b, `{"tag_name":%q,"assets":[`, f.tag)
		first := true
		for name := range f.assets {
			if !first {
				b.WriteString(",")
			}
			first = false
			fmt.Fprintf(&b, `{"name":%q,"browser_download_url":%q}`, name, srv.URL+"/dl/"+name)
		}
		if !f.omitSum {
			if !first {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"name":"checksums.txt","browser_download_url":%q}`, srv.URL+"/dl/checksums.txt")
		}
		b.WriteString("]}")
		_, _ = w.Write([]byte(b.String()))
	})

	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		if name == "checksums.txt" {
			var b strings.Builder
			for n, data := range f.assets {
				sum := sha256.Sum256(data)
				h := hex.EncodeToString(sum[:])
				if f.badSum {
					h = strings.Repeat("0", 64)
				}
				fmt.Fprintf(&b, "%s  %s\n", h, n)
			}
			_, _ = w.Write([]byte(b.String()))
			return
		}
		data, ok := f.assets[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	})
	return srv
}

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipped(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// installed returns a temp file standing in for the running executable.
func installed(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ypotitlo")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func opts(srv *httptest.Server, exe, current string) Options {
	return Options{
		CurrentVersion: current,
		Repo:           "owner/name",
		APIBase:        srv.URL,
		HTTPClient:     srv.Client(),
		GOOS:           "linux",
		GOARCH:         "amd64",
		ExePath:        exe,
	}
}

func TestUpgradeReplacesTheBinary(t *testing.T) {
	t.Parallel()

	rel := &fakeRelease{tag: "v0.2.0", assets: map[string][]byte{
		"ypotitlo_0.2.0_linux_amd64.tar.gz": tarGz(t, "ypotitlo", []byte("NEW BINARY")),
	}}
	srv := rel.server(t)
	exe := installed(t, "OLD BINARY")

	var out bytes.Buffer
	res, err := Run(context.Background(), &out, opts(srv, exe, "v0.1.0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Upgraded || res.Latest != "v0.2.0" {
		t.Errorf("result = %+v", res)
	}

	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW BINARY" {
		t.Errorf("installed binary = %q, want the downloaded one", got)
	}
	fi, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Errorf("mode = %o, want 755: the replacement must stay executable", perm)
	}
}

// A .zip asset must work too. The release publishes zips for Windows, and
// matching only tarballs would leave upgrade silently unavailable there.
func TestUpgradeAcceptsAZipAsset(t *testing.T) {
	t.Parallel()

	rel := &fakeRelease{tag: "v0.2.0", assets: map[string][]byte{
		"ypotitlo_0.2.0_windows_amd64.zip": zipped(t, "ypotitlo.exe", []byte("WINDOWS BINARY")),
	}}
	srv := rel.server(t)
	exe := installed(t, "OLD")

	o := opts(srv, exe, "v0.1.0")
	o.GOOS, o.GOARCH = "windows", "amd64"

	var out bytes.Buffer
	if _, err := Run(context.Background(), &out, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "WINDOWS BINARY" {
		t.Errorf("installed = %q", got)
	}
}

func TestUpgradeIsANoOpWhenCurrent(t *testing.T) {
	t.Parallel()

	rel := &fakeRelease{tag: "v0.1.0", assets: map[string][]byte{
		"ypotitlo_0.1.0_linux_amd64.tar.gz": tarGz(t, "ypotitlo", []byte("NEW")),
	}}
	srv := rel.server(t)
	exe := installed(t, "OLD")

	var out bytes.Buffer
	res, err := Run(context.Background(), &out, opts(srv, exe, "v0.1.0"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Upgraded {
		t.Error("reported an upgrade when already current")
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD" {
		t.Errorf("binary was replaced: %q", got)
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Errorf("output = %q", out.String())
	}
}

// A newer local build must not be downgraded to the published release.
func TestUpgradeDoesNotDowngrade(t *testing.T) {
	t.Parallel()

	rel := &fakeRelease{tag: "v0.1.0", assets: map[string][]byte{
		"ypotitlo_0.1.0_linux_amd64.tar.gz": tarGz(t, "ypotitlo", []byte("OLDER")),
	}}
	srv := rel.server(t)
	exe := installed(t, "NEWER")

	var out bytes.Buffer
	if _, err := Run(context.Background(), &out, opts(srv, exe, "v0.9.0")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "NEWER" {
		t.Errorf("downgraded to %q", got)
	}
}

// A mismatched checksum must abort before anything is replaced.
func TestUpgradeRefusesABadChecksum(t *testing.T) {
	t.Parallel()

	rel := &fakeRelease{tag: "v0.2.0", badSum: true, assets: map[string][]byte{
		"ypotitlo_0.2.0_linux_amd64.tar.gz": tarGz(t, "ypotitlo", []byte("TAMPERED")),
	}}
	srv := rel.server(t)
	exe := installed(t, "OLD")

	var out bytes.Buffer
	_, err := Run(context.Background(), &out, opts(srv, exe, "v0.1.0"))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %v, want a checksum mismatch", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD" {
		t.Errorf("binary was replaced despite a bad checksum: %q", got)
	}
}

// A release with no checksums.txt is refused rather than installed unverified.
func TestUpgradeRefusesWithoutChecksums(t *testing.T) {
	t.Parallel()

	rel := &fakeRelease{tag: "v0.2.0", omitSum: true, assets: map[string][]byte{
		"ypotitlo_0.2.0_linux_amd64.tar.gz": tarGz(t, "ypotitlo", []byte("UNVERIFIED")),
	}}
	srv := rel.server(t)
	exe := installed(t, "OLD")

	var out bytes.Buffer
	_, err := Run(context.Background(), &out, opts(srv, exe, "v0.1.0"))
	if err == nil || !strings.Contains(err.Error(), "checksums") {
		t.Fatalf("error = %v, want a refusal about checksums", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD" {
		t.Errorf("binary was replaced: %q", got)
	}
}

func TestUpgradeReportsAMissingPlatformAsset(t *testing.T) {
	t.Parallel()

	rel := &fakeRelease{tag: "v0.2.0", assets: map[string][]byte{
		"ypotitlo_0.2.0_linux_amd64.tar.gz": tarGz(t, "ypotitlo", []byte("NEW")),
	}}
	srv := rel.server(t)
	exe := installed(t, "OLD")

	o := opts(srv, exe, "v0.1.0")
	o.GOOS, o.GOARCH = "plan9", "riscv64"

	var out bytes.Buffer
	_, err := Run(context.Background(), &out, o)
	if err == nil || !strings.Contains(err.Error(), "plan9/riscv64") {
		t.Fatalf("error = %v, want it to name the platform", err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD" {
		t.Errorf("binary was replaced: %q", got)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	t.Parallel()

	rel := &fakeRelease{tag: "v0.2.0", assets: map[string][]byte{
		"ypotitlo_0.2.0_linux_amd64.tar.gz": tarGz(t, "ypotitlo", []byte("NEW")),
	}}
	srv := rel.server(t)
	exe := installed(t, "OLD")

	o := opts(srv, exe, "v0.1.0")
	o.DryRun = true

	var out bytes.Buffer
	res, err := Run(context.Background(), &out, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Upgraded {
		t.Error("a dry run reported an upgrade")
	}
	if got, _ := os.ReadFile(exe); string(got) != "OLD" {
		t.Errorf("a dry run replaced the binary: %q", got)
	}
	if !strings.Contains(out.String(), "would download") {
		t.Errorf("output = %q", out.String())
	}
}

// "dev" and other unparseable versions count as upgradeable, since a local build
// has no way to prove it is current.
func TestNewer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.1.0", "v0.1.1", true},
		{"v0.9.0", "v1.0.0", true},
		{"v0.2.0", "v0.1.0", false},
		{"v0.1.0", "v0.1.0", false},
		{"0.1.0", "v0.2.0", true},     // missing prefix on either side
		{"dev", "v0.1.0", true},       // a local build is always upgradeable
		{"", "v0.1.0", true},          // as is an unstamped one
		{"v0.1.0", "nonsense", false}, // a malformed tag never triggers a replacement
		// A git-describe build of the released commit must not "upgrade" to it.
		{"v0.2.0-3-gabc1234", "v0.2.0", false},
		{"v0.2.0-3-gabc1234", "v0.2.1", true},
	}

	for _, tt := range tests {
		if got := newer(tt.current, tt.latest); got != tt.want {
			t.Errorf("newer(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
		}
	}
}
