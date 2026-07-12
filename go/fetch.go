package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// defaultFetchDestination is a portable, relative docs root under the current working
// directory. It is deliberately not an absolute machine path so the tool stays project- and
// workstation-independent; pass --destination to place the docs anywhere.
const defaultFetchDestination = "unity-docs"

// sectionDirs are the documentation subtrees the corpus builder consumes (their nested
// docdata/ is included because it holds page titles). Everything else in Unity's offline zip
// - StaticFiles, StaticFilesManual, uploads, top-level images - is read by neither the build
// step nor the source-verification path, so fetch skips it entirely. Extracting only these
// subtrees is the bulk of the speedup: no unwanted bytes are written, and there is no second
// copy pass.
var sectionDirs = []string{"Manual", "ScriptReference"}

var unityVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// pinnedDocHosts is the closed set of hosts fetch may contact on any hop. Unity publishes
// no checksums for the docs zip, so TLS to these hosts is the only integrity control on
// the download - a redirect off them must fail, not be followed.
var pinnedDocHosts = map[string]bool{
	"docs.unity3d.com":            true,
	"cloudmedia-docs.unity3d.com": true,
	"storage.googleapis.com":      true,
}

func checkPinnedRedirect(req *http.Request, _ []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("redirect off https refused: %s", req.URL)
	}
	if !pinnedDocHosts[req.URL.Hostname()] {
		return fmt.Errorf("redirect to unpinned host refused: %s", req.URL)
	}
	// storage.googleapis.com is a shared multi-tenant host: pin the docscloudstorage
	// bucket, not just the host, so a hop cannot land in an arbitrary GCS bucket. The
	// unity3d.com hosts are Unity's own; the host pin is the control there.
	if req.URL.Hostname() == "storage.googleapis.com" && !strings.HasPrefix(req.URL.EscapedPath(), "/docscloudstorage/") {
		return fmt.Errorf("redirect outside the docscloudstorage bucket refused: %s", req.URL)
	}
	return nil
}

var httpClient = &http.Client{
	Timeout:       30 * time.Minute,
	CheckRedirect: checkPinnedRedirect,
}

// fetchMarkerName marks a directory as created by fetch. It carries the fetched Unity
// version (surfaced by build into manifest.json) and the zip provenance, and it is the
// only kind of directory `fetch --force` will delete.
const fetchMarkerName = ".unity-doc-fetch"

type fetchInfo struct {
	UnityVersion string `json:"unity_version"`
	ZipURL       string `json:"zip_url"`
	ZipSHA256    string `json:"zip_sha256"`
	FetchedAtUTC string `json:"fetched_at_utc"`
	// ZipName is the retained zip's filename inside the destination directory. Empty when
	// the zip was deleted (--delete-zip) or the marker predates zip retention.
	ZipName string `json:"zip_name,omitempty"`
}

type fetchCacheInfo struct {
	ZipURL    string `json:"zip_url"`
	ZipSHA256 string `json:"zip_sha256"`
}

func readFetchInfo(root string) (fetchInfo, bool) {
	data, err := os.ReadFile(filepath.Join(root, fetchMarkerName))
	if err != nil {
		return fetchInfo{}, false
	}
	var info fetchInfo
	if json.Unmarshal(data, &info) != nil {
		return fetchInfo{}, false
	}
	if validateFetchInfo(info) != nil {
		return fetchInfo{}, false
	}
	return info, true
}

func validateFetchInfo(info fetchInfo) error {
	if !unityVersionRe.MatchString(info.UnityVersion) {
		return fmt.Errorf("invalid unity_version %q", info.UnityVersion)
	}
	if zipURLRe.FindString(info.ZipURL) != info.ZipURL {
		return fmt.Errorf("invalid or unpinned zip_url %q", info.ZipURL)
	}
	if !strings.HasSuffix(info.ZipURL, "/"+info.UnityVersion+"/UnityDocumentation.zip") {
		return fmt.Errorf("zip_url does not match unity_version %q", info.UnityVersion)
	}
	if len(info.ZipSHA256) != sha256.Size*2 {
		return fmt.Errorf("invalid zip_sha256")
	}
	if _, err := hex.DecodeString(info.ZipSHA256); err != nil {
		return fmt.Errorf("invalid zip_sha256: %w", err)
	}
	if _, err := time.Parse(time.RFC3339, info.FetchedAtUTC); err != nil {
		return fmt.Errorf("invalid fetched_at_utc: %w", err)
	}
	if info.ZipName != "" && info.ZipName != info.UnityVersion+"-UnityDocumentation.zip" {
		return fmt.Errorf("invalid zip_name %q", info.ZipName)
	}
	return nil
}

func cacheInfoPath(zipPath string) string { return zipPath + ".provenance.json" }

func writeCacheInfo(zipPath, zipURL, zipSHA string) error {
	data, err := json.MarshalIndent(fetchCacheInfo{ZipURL: zipURL, ZipSHA256: zipSHA}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cacheInfoPath(zipPath), append(data, '\n'), 0o644)
}

func validateCachedZip(zipPath, expectedURL string) (string, error) {
	data, err := os.ReadFile(cacheInfoPath(zipPath))
	if err != nil {
		return "", fmt.Errorf("cached zip %s has no trusted download provenance; remove it and retry: %w", zipPath, err)
	}
	var info fetchCacheInfo
	if err := json.Unmarshal(data, &info); err != nil || info.ZipURL != expectedURL || len(info.ZipSHA256) != sha256.Size*2 {
		return "", fmt.Errorf("cached zip %s has invalid or mismatched download provenance; remove it and retry", zipPath)
	}
	actual, err := hashFileSHA256(zipPath)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(actual, info.ZipSHA256) {
		return "", fmt.Errorf("cached zip %s SHA-256 %s does not match its trusted provenance %s; remove it and retry", zipPath, actual, info.ZipSHA256)
	}
	return actual, nil
}

// Offline docs zips live in Unity's docscloudstorage bucket, served from two hosts: the
// cloudmedia CDN for current streams (2020.3+) and the GCS bucket directly for older ones
// (2019.4 and older link storage.googleapis.com from their OfflineDocumentation pages).
var zipURLRe = regexp.MustCompile(`https://(?:cloudmedia-docs\.unity3d\.com/docscloudstorage/en|storage\.googleapis\.com/docscloudstorage)/[^"]+/UnityDocumentation\.zip`)

func runFetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	version := fs.String("version", "", "Unity major.minor documentation stream, e.g. 6000.3.")
	destination := fs.String("destination", defaultFetchDestination, "Directory to populate with Manual/ScriptReference docs.")
	cacheRoot := fs.String("cache-root", "", "Download cache dir. Defaults to <os-temp-dir>/unity-doc-downloads.")
	workers := fs.Int("workers", 0, "Parallel extraction workers. Defaults to the number of logical CPUs.")
	force := fs.Bool("force", false, "Replace an existing destination directory.")
	deleteZip := fs.Bool("delete-zip", false, "Delete the zip after extraction instead of keeping it in the destination (the retained zip is the rebuild/verification artifact).")
	resolveOnly := fs.Bool("resolve-only", false, "Print the resolved zip URL and exit without downloading.")
	_ = fs.Parse(args)
	if *version == "" {
		fmt.Fprintln(os.Stderr, "Usage: unity-doc-corpus fetch --version <ver> [--destination <dir>] [--cache-root <dir>] [--workers N] [--force] [--delete-zip] [--resolve-only]")
		os.Exit(2)
	}
	if err := fetch(*version, *destination, *cacheRoot, *workers, *force, *deleteZip, *resolveOnly); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func fetch(version, destination, cacheRoot string, workers int, force, deleteZip, resolveOnly bool) error {
	if !unityVersionRe.MatchString(version) {
		return fmt.Errorf("invalid Unity version %q: expected major.minor digits, for example 6000.3", version)
	}
	if workers < 1 {
		workers = runtime.NumCPU()
	}
	zipURL, err := resolveZipURL(version)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Offline docs zip:", zipURL)
	if resolveOnly {
		fmt.Println(zipURL)
		return nil
	}

	destAbs, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if cacheRoot == "" {
		cacheRoot = filepath.Join(os.TempDir(), "unity-doc-downloads")
	}
	cacheAbs, err := filepath.Abs(cacheRoot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cacheAbs, 0o755); err != nil {
		return err
	}
	zipPath := filepath.Join(cacheAbs, version+"-UnityDocumentation.zip")
	// A --force re-fetch is about to delete the destination; salvage its retained zip into
	// the cache first so the same version is not downloaded again.
	if force {
		if err := salvageRetainedZip(destAbs, zipPath, version, zipURL); err != nil {
			return err
		}
	}
	if err := prepareFetchDestination(destAbs, force); err != nil {
		return err
	}
	var zipSHA string
	if _, err := os.Stat(zipPath); err != nil {
		fmt.Fprintf(os.Stderr, "Downloading to %s\n", zipPath)
		zipSHA, err = downloadFile(zipURL, zipPath)
		if err != nil {
			return err
		}
		if err := writeCacheInfo(zipPath, zipURL, zipSHA); err != nil {
			return fmt.Errorf("recording cached zip provenance: %w", err)
		}
	} else {
		fmt.Fprintln(os.Stderr, "Using cached zip:", zipPath)
		zipSHA, err = validateCachedZip(zipPath, zipURL)
		if err != nil {
			return err
		}
	}
	fmt.Fprintln(os.Stderr, "Zip SHA-256:", zipSHA)
	if err := os.MkdirAll(destAbs, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Extracting %s to %s (%d workers)\n", strings.Join(sectionDirs, " + "), destAbs, workers)
	if err := extractSections(zipPath, destAbs, workers); err != nil {
		return err
	}

	// The pre-selective pipeline expanded the whole zip into <ver>-expanded before copying;
	// current fetch never creates it, so remove any leftover to keep the cache lean.
	os.RemoveAll(filepath.Join(cacheAbs, version+"-expanded"))
	// The zip is the retained ground-truth artifact: build can rematerialize the extracted
	// tree from it and the source verb can read original pages out of it, so it moves into
	// the destination (the OS temp cache is not a safe long-term home). --delete-zip drops
	// it to reclaim ~475 MB, leaving online canonical_url lookups as the verification path.
	zipName := ""
	if deleteZip {
		if err := os.Remove(zipPath); err == nil {
			fmt.Fprintln(os.Stderr, "Removed zip (--delete-zip):", zipPath)
		}
	} else {
		zipName = filepath.Base(zipPath)
		if err := moveFile(zipPath, filepath.Join(destAbs, zipName)); err != nil {
			return fmt.Errorf("retaining zip in destination: %w", err)
		}
		fmt.Fprintln(os.Stderr, "Retained zip:", filepath.Join(destAbs, zipName))
	}

	info := fetchInfo{UnityVersion: version, ZipURL: zipURL, ZipSHA256: zipSHA, FetchedAtUTC: time.Now().UTC().Format(time.RFC3339), ZipName: zipName}
	infoBytes, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(destAbs, fetchMarkerName), append(infoBytes, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "Done. Docs extracted to:", destAbs)
	fmt.Fprintf(os.Stderr, "Next: bin/unity-doc-corpus build --source %s --output %s\n", destination, destination+"/_agent")
	return nil
}

func salvageRetainedZip(destAbs, cacheZip, version, resolvedURL string) error {
	info, ok := readFetchInfo(destAbs)
	if !ok || info.ZipName == "" || info.UnityVersion != version {
		return nil
	}
	retained := filepath.Join(destAbs, info.ZipName)
	if _, err := os.Stat(retained); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("checking retained zip before reuse: %w", err)
	}
	retainedSHA, err := hashFileSHA256(retained)
	if err != nil {
		return fmt.Errorf("checking retained zip before reuse: %w", err)
	}
	if !strings.EqualFold(retainedSHA, info.ZipSHA256) {
		return fmt.Errorf("refusing to reuse retained zip %s: SHA-256 %s does not match marker %s", retained, retainedSHA, info.ZipSHA256)
	}
	if _, err := os.Stat(cacheZip); os.IsNotExist(err) {
		if err := writeCacheInfo(cacheZip, resolvedURL, info.ZipSHA256); err != nil {
			return fmt.Errorf("recording salvaged zip provenance: %w", err)
		}
		if err := moveFile(retained, cacheZip); err != nil {
			os.Remove(cacheInfoPath(cacheZip))
			return fmt.Errorf("moving retained zip into cache: %w", err)
		}
		fmt.Fprintln(os.Stderr, "Reusing retained zip from destination:", retained)
		return nil
	}
	cachedSHA, err := hashFileSHA256(cacheZip)
	if err != nil {
		return err
	}
	if !strings.EqualFold(cachedSHA, info.ZipSHA256) {
		return fmt.Errorf("refusing cached zip %s: SHA-256 %s does not match destination marker %s", cacheZip, cachedSHA, info.ZipSHA256)
	}
	if err := writeCacheInfo(cacheZip, resolvedURL, info.ZipSHA256); err != nil {
		return fmt.Errorf("recording cached zip provenance: %w", err)
	}
	return nil
}

// prepareFetchDestination clears an existing destination only when --force is set AND the
// directory carries the fetch marker proving this tool created it - the same guard shape
// build uses for its output. Anything else is refused, never deleted.
func prepareFetchDestination(destAbs string, force bool) error {
	if _, err := os.Stat(destAbs); err != nil {
		return nil
	}
	if !force {
		return fmt.Errorf("destination already exists, pass --force to replace it: %s", destAbs)
	}
	if _, ok := readFetchInfo(destAbs); !ok {
		return fmt.Errorf("refusing to replace %s: it has no valid %s marker, so fetch cannot prove ownership - delete it yourself if you really mean it", destAbs, fetchMarkerName)
	}
	return os.RemoveAll(destAbs)
}

func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// resolveZipURL fetches Unity's offline-documentation page and scrapes the real zip URL,
// falling back to the deterministic constructed URL when the page is unreachable or the link
// is absent. The download step still validates the URL, so a wrong guess fails loudly there.
func resolveZipURL(version string) (string, error) {
	pageURL := fmt.Sprintf("https://docs.unity3d.com/%s/Documentation/Manual/OfflineDocumentation.html", version)
	// Both candidate locations are inside Unity's docscloudstorage bucket; some old streams
	// (e.g. 5.6) have no OfflineDocumentation page anymore but still serve the zip from GCS.
	candidates := []string{
		fmt.Sprintf("https://cloudmedia-docs.unity3d.com/docscloudstorage/en/%s/UnityDocumentation.zip", version),
		fmt.Sprintf("https://storage.googleapis.com/docscloudstorage/%s/UnityDocumentation.zip", version),
	}
	fmt.Fprintln(os.Stderr, "Resolving", pageURL)
	resp, err := httpClient.Get(pageURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: resolution page unreachable, probing known zip locations:", err)
		return probeZipCandidates(candidates)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "warning: resolution page returned HTTP %d, probing known zip locations\n", resp.StatusCode)
		return probeZipCandidates(candidates)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not read resolution page, probing known zip locations:", err)
		return probeZipCandidates(candidates)
	}
	if m := zipURLRe.Find(body); m != nil {
		return string(m), nil
	}
	return probeZipCandidates(candidates)
}

// probeZipCandidates HEAD-checks the known zip locations in order and returns the first that
// exists. When none respond it still returns the first candidate so the download step fails
// loudly with a concrete URL instead of hiding the problem here.
func probeZipCandidates(candidates []string) (string, error) {
	for _, u := range candidates {
		resp, err := httpClient.Head(u)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return u, nil
		}
	}
	fmt.Fprintln(os.Stderr, "warning: no known zip location responded for this version; trying", candidates[0])
	return candidates[0], nil
}

func downloadFile(url, dest string) (string, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s -> HTTP %d", url, resp.StatusCode)
	}
	if resp.ContentLength > 0 {
		fmt.Fprintf(os.Stderr, "Download size: %d MB\n", resp.ContentLength/(1000*1000))
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, hasher), resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// extractSections extracts only the Manual and ScriptReference subtrees from the offline docs
// zip, strips the doc-root prefix so they land at the top of dest, and writes files in
// parallel. It replaces the former unzip-everything-then-copy pipeline: nothing is written
// twice and the unused ~half of the archive (static assets, uploads) is never touched.
func extractSections(zipPath, dest string, workers int) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	prefix, err := findSectionPrefix(r.File)
	if err != nil {
		return err
	}

	type job struct {
		file   *zip.File
		target string
	}
	var jobs []job
	targets := map[string]string{}
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel := strings.TrimPrefix(filepath.ToSlash(f.Name), prefix)
		if !underSection(rel) {
			continue
		}
		target := filepath.Join(destAbs, filepath.FromSlash(rel))
		// Zip-slip guard: reject entries that resolve outside the destination.
		if target != destAbs && !strings.HasPrefix(target, destAbs+string(os.PathSeparator)) {
			return fmt.Errorf("zip entry escapes destination: %s", f.Name)
		}
		key := strings.ToLower(filepath.ToSlash(target))
		if prior, ok := targets[key]; ok {
			return fmt.Errorf("zip entries %q and %q resolve to the same output path %s", prior, f.Name, target)
		}
		targets[key] = f.Name
		jobs = append(jobs, job{file: f, target: target})
	}
	if len(jobs) == 0 {
		return fmt.Errorf("no %s entries found under %q in %s", strings.Join(sectionDirs, "/"), prefix, zipPath)
	}

	if workers < 1 {
		workers = 1
	}
	var (
		mu       sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}
	jobCh := make(chan job)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				if failed() {
					continue // keep draining so the producer never blocks
				}
				if err := writeZipEntry(j.file, j.target); err != nil {
					fail(err)
				}
			}
		}()
	}
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)
	wg.Wait()
	return firstErr
}

// findSectionPrefix returns the path prefix (with a trailing slash, or empty for a
// zip-root layout) under which every entry in sectionDirs lives - the analogue of the
// documentation root the builder expects, computed from zip entry names without extracting.
func findSectionPrefix(files []*zip.File) (string, error) {
	// prefix -> set of sectionDirs that have a child entry under it.
	seen := map[string]map[string]bool{}
	for _, f := range files {
		segs := strings.Split(filepath.ToSlash(f.Name), "/")
		for i, seg := range segs {
			// The section segment must have a child (i+1 < len) so we only count
			// prefixes where the section directory actually holds content.
			if i+1 >= len(segs) {
				continue
			}
			for _, section := range sectionDirs {
				if seg != section {
					continue
				}
				prefix := strings.Join(segs[:i], "/")
				if prefix != "" {
					prefix += "/"
				}
				if seen[prefix] == nil {
					seen[prefix] = map[string]bool{}
				}
				seen[prefix][section] = true
			}
		}
	}
	best := ""
	found := false
	for prefix, sections := range seen {
		complete := true
		for _, section := range sectionDirs {
			if !sections[section] {
				complete = false
				break
			}
		}
		if complete && (!found || len(prefix) < len(best)) {
			best, found = prefix, true
		}
	}
	if !found {
		return "", fmt.Errorf("zip does not contain a directory holding all of: %s", strings.Join(sectionDirs, ", "))
	}
	return best, nil
}

// underSection reports whether a prefix-stripped, slash-separated path lies inside one of the
// wanted section subtrees.
func underSection(rel string) bool {
	for _, section := range sectionDirs {
		if strings.HasPrefix(rel, section+"/") {
			return true
		}
	}
	return false
}

// moveFile renames src to dst, falling back to copy+delete when they sit on different
// volumes (the download cache defaults to the OS temp dir, which may not share a volume
// with the destination).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	in.Close()
	return os.Remove(src)
}

func writeZipEntry(f *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
