// Package selfupdate replaces the running binary with the newest published
// release.
//
// It compares the installed version against the latest GitHub release,
// downloads the archive for this platform, verifies its checksum against the
// release's checksums.txt, and swaps the executable atomically. A failure at any
// step leaves the installed binary untouched — the replacement only happens once
// the new one is on disk and verified, which is the same rule the translator
// follows for subtitle files.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRepo    = "mtzanidakis/ypotitlo"
	defaultAPIBase = "https://api.github.com"
	binaryName     = "ypotitlo"

	// maxArchive bounds what will be pulled into memory. The real archives are
	// a few megabytes; anything far larger is a wrong URL or a hostile one.
	maxArchive = 64 << 20
)

// Options configures an upgrade. Every field that touches the outside world is
// overridable so the whole flow can be pointed at a test server and a temp file.
type Options struct {
	CurrentVersion string
	Repo           string // "owner/name"
	APIBase        string
	HTTPClient     *http.Client
	GOOS           string
	GOARCH         string
	ExePath        string // executable to replace
	DryRun         bool   // report what would happen, change nothing
}

// Result describes what an upgrade did or would do.
type Result struct {
	Current  string
	Latest   string
	Upgraded bool
	Asset    string
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// Run upgrades the running binary when a newer release exists.
//
// Progress goes to out. Being already current is success, not an error.
func Run(ctx context.Context, out io.Writer, opts Options) (Result, error) {
	opts.applyDefaults()

	rel, err := opts.latestRelease(ctx)
	if err != nil {
		return Result{}, err
	}
	latest := strings.TrimSpace(rel.TagName)
	if latest == "" {
		return Result{}, fmt.Errorf("the latest release has no tag")
	}

	res := Result{Current: display(opts.CurrentVersion), Latest: latest}
	_, _ = fmt.Fprintf(out, "installed %s, latest %s\n", res.Current, latest)

	if !newer(opts.CurrentVersion, latest) {
		_, _ = fmt.Fprintln(out, "already up to date")
		return res, nil
	}

	archive, ok := findAsset(rel, opts.GOOS, opts.GOARCH)
	if !ok {
		return res, fmt.Errorf("the %s release has no asset for %s/%s", latest, opts.GOOS, opts.GOARCH)
	}
	res.Asset = archive.Name

	if opts.DryRun {
		_, _ = fmt.Fprintf(out, "would download %s and replace %s\n", archive.Name, opts.ExePath)
		return res, nil
	}
	if opts.ExePath == "" {
		return res, fmt.Errorf("cannot locate the running executable to replace")
	}

	_, _ = fmt.Fprintf(out, "downloading %s\n", archive.Name)
	data, err := opts.download(ctx, archive.URL)
	if err != nil {
		return res, err
	}

	// A missing checksums.txt is not a reason to install an unverified binary.
	sums, ok := findChecksums(rel)
	if !ok {
		return res, fmt.Errorf("the %s release publishes no checksums.txt; refusing to install unverified", latest)
	}
	if err := opts.verifyChecksum(ctx, sums.URL, archive.Name, data); err != nil {
		return res, err
	}

	bin, err := extractBinary(data, archive.Name)
	if err != nil {
		return res, err
	}
	if err := replaceExecutable(opts.ExePath, bin, opts.GOOS); err != nil {
		return res, err
	}

	res.Upgraded = true
	_, _ = fmt.Fprintf(out, "upgraded to %s\n", latest)
	return res, nil
}

func (o *Options) applyDefaults() {
	if o.Repo == "" {
		o.Repo = defaultRepo
	}
	if o.APIBase == "" {
		o.APIBase = defaultAPIBase
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 2 * time.Minute}
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOARCH == "" {
		o.GOARCH = runtime.GOARCH
	}
	if o.ExePath == "" {
		if p, err := os.Executable(); err == nil {
			// Resolve symlinks so that upgrading a binary reached through one
			// replaces the target rather than clobbering the link.
			if rp, err := filepath.EvalSymlinks(p); err == nil {
				p = rp
			}
			o.ExePath = p
		}
	}
}

func (o *Options) get(ctx context.Context, url, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("User-Agent", binaryName)

	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%s: not found", path.Base(url))
	default:
		return nil, fmt.Errorf("%s: http %d", path.Base(url), resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxArchive))
}

func (o *Options) latestRelease(ctx context.Context) (*ghRelease, error) {
	url := strings.TrimRight(o.APIBase, "/") + "/repos/" + o.Repo + "/releases/latest"
	body, err := o.get(ctx, url, "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("looking up the latest release: %w", err)
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("decoding the release: %w", err)
	}
	return &rel, nil
}

func (o *Options) download(ctx context.Context, url string) ([]byte, error) {
	return o.get(ctx, url, "")
}

func (o *Options) verifyChecksum(ctx context.Context, sumsURL, name string, data []byte) error {
	raw, err := o.download(ctx, sumsURL)
	if err != nil {
		return fmt.Errorf("fetching checksums: %w", err)
	}
	want := ""
	for line := range strings.SplitSeq(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			want = f[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt has no entry for %s", name)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s", name)
	}
	return nil
}

// findAsset picks the archive for a platform. Both archive formats are matched
// because the release publishes .tar.gz everywhere except Windows, which gets a
// .zip — looking only for tarballs would make upgrade silently unavailable there.
func findAsset(rel *ghRelease, goos, goarch string) (ghAsset, bool) {
	for _, a := range rel.Assets {
		if !isArchive(a.Name) {
			continue
		}
		if strings.Contains(a.Name, goos) && strings.Contains(a.Name, goarch) {
			return a, true
		}
	}
	return ghAsset{}, false
}

func isArchive(name string) bool {
	return strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".zip")
}

func findChecksums(rel *ghRelease) (ghAsset, bool) {
	for _, a := range rel.Assets {
		if a.Name == "checksums.txt" {
			return a, true
		}
	}
	return ghAsset{}, false
}

// extractBinary pulls the executable out of the archive, dispatching on the
// asset's extension.
func extractBinary(archive []byte, assetName string) ([]byte, error) {
	if strings.HasSuffix(assetName, ".zip") {
		return fromZip(archive)
	}
	return fromTarGz(archive)
}

func fromTarGz(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the archive: %w", err)
		}
		if h.Typeflag == tar.TypeReg && isBinary(h.Name) {
			return io.ReadAll(io.LimitReader(tr, maxArchive))
		}
	}
	return nil, fmt.Errorf("no %s executable inside the archive", binaryName)
}

func fromZip(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("unzip: %w", err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !isBinary(f.Name) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		return io.ReadAll(io.LimitReader(rc, maxArchive))
	}
	return nil, fmt.Errorf("no %s executable inside the archive", binaryName)
}

func isBinary(name string) bool {
	base := filepath.Base(filepath.ToSlash(name))
	return base == binaryName || base == binaryName+".exe"
}

// replaceExecutable writes the new binary beside the current one and renames it
// into place, so the swap is atomic and a failure partway leaves the old binary
// running. Windows cannot overwrite a running image, so the old one is moved
// aside first.
func replaceExecutable(exePath string, newBin []byte, goos string) error {
	if len(newBin) == 0 {
		return fmt.Errorf("the downloaded binary is empty")
	}
	dir := filepath.Dir(exePath)

	tmp, err := os.CreateTemp(dir, ".ypotitlo-upgrade-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // a no-op once renamed

	if _, err := tmp.Write(newBin); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if goos == "windows" {
		_ = os.Rename(exePath, exePath+".old")
	}
	if err := os.Rename(tmpName, exePath); err != nil {
		return fmt.Errorf("replacing %s: %w", exePath, err)
	}
	return nil
}

func display(v string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return "dev"
}

// newer compares the MAJOR.MINOR.PATCH cores, ignoring any pre-release or
// git-describe suffix. An unparseable current version — "dev", a local build —
// counts as upgradeable; an unparseable latest never does, so a malformed tag
// cannot trigger a replacement.
func newer(current, latest string) bool {
	lc, ok := parseCore(latest)
	if !ok {
		return false
	}
	cc, ok := parseCore(current)
	if !ok {
		return true
	}
	for i := range 3 {
		if lc[i] != cc[i] {
			return lc[i] > cc[i]
		}
	}
	return false
}

func parseCore(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out [3]int
	parts := strings.Split(v, ".")
	if parts[0] == "" {
		return out, false
	}
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
