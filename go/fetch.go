package main

import (
	"archive/zip"
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

var httpClient = &http.Client{Timeout: 30 * time.Minute}

var zipURLRe = regexp.MustCompile(`https://cloudmedia-docs\.unity3d\.com/docscloudstorage/en/[^"]+/UnityDocumentation\.zip`)

func runFetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	version := fs.String("version", "", "Unity major.minor documentation stream, e.g. 6000.3.")
	destination := fs.String("destination", defaultFetchDestination, "Directory to populate with Manual/ScriptReference docs.")
	cacheRoot := fs.String("cache-root", "", "Download cache dir. Defaults to <os-temp-dir>/unity-doc-downloads.")
	workers := fs.Int("workers", 0, "Parallel extraction workers. Defaults to the number of logical CPUs.")
	force := fs.Bool("force", false, "Replace an existing destination directory.")
	keepZip := fs.Bool("keep-zip", false, "Keep the downloaded zip in the cache after extraction (default: delete it to reclaim space).")
	resolveOnly := fs.Bool("resolve-only", false, "Print the resolved zip URL and exit without downloading.")
	_ = fs.Parse(args)
	if *version == "" {
		fmt.Fprintln(os.Stderr, "Usage: unity-doc-corpus fetch --version <ver> [--destination <dir>] [--cache-root <dir>] [--workers N] [--force] [--keep-zip] [--resolve-only]")
		os.Exit(2)
	}
	if err := fetch(*version, *destination, *cacheRoot, *workers, *force, *keepZip, *resolveOnly); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func fetch(version, destination, cacheRoot string, workers int, force, keepZip, resolveOnly bool) error {
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
	if _, err := os.Stat(destAbs); err == nil && !force {
		return fmt.Errorf("destination already exists, pass --force to replace it: %s", destAbs)
	}
	if err := os.MkdirAll(cacheAbs, 0o755); err != nil {
		return err
	}

	zipPath := filepath.Join(cacheAbs, version+"-UnityDocumentation.zip")
	if _, err := os.Stat(zipPath); err != nil {
		fmt.Fprintf(os.Stderr, "Downloading to %s\n", zipPath)
		if err := downloadFile(zipURL, zipPath); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, "Using cached zip:", zipPath)
	}

	if force {
		if err := os.RemoveAll(destAbs); err != nil {
			return err
		}
	}
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
	// The zip is only needed during extraction - build reads the extracted docs, not the
	// archive - so delete it by default to reclaim ~475 MB. --keep-zip retains the cache for
	// a later re-fetch (e.g. reusing one download across several builds).
	if !keepZip {
		if err := os.Remove(zipPath); err == nil {
			fmt.Fprintln(os.Stderr, "Removed cached zip (pass --keep-zip to retain):", zipPath)
		}
	}

	fmt.Fprintln(os.Stderr, "Done. Docs extracted to:", destAbs)
	fmt.Fprintf(os.Stderr, "Next: bin/unity-doc-corpus build --source %s --output %s\n", destination, destination+"/_agent")
	return nil
}

// resolveZipURL fetches Unity's offline-documentation page and scrapes the real zip URL,
// falling back to the deterministic constructed URL when the page is unreachable or the link
// is absent. The download step still validates the URL, so a wrong guess fails loudly there.
func resolveZipURL(version string) (string, error) {
	pageURL := fmt.Sprintf("https://docs.unity3d.com/%s/Documentation/Manual/OfflineDocumentation.html", version)
	fallback := fmt.Sprintf("https://cloudmedia-docs.unity3d.com/docscloudstorage/en/%s/UnityDocumentation.zip", version)
	fmt.Fprintln(os.Stderr, "Resolving", pageURL)
	resp, err := httpClient.Get(pageURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: resolution page unreachable, using fallback URL:", err)
		return fallback, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "warning: resolution page returned HTTP %d, using fallback URL\n", resp.StatusCode)
		return fallback, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not read resolution page, using fallback URL:", err)
		return fallback, nil
	}
	if m := zipURLRe.Find(body); m != nil {
		return string(m), nil
	}
	return fallback, nil
}

func downloadFile(url, dest string) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s -> HTTP %d", url, resp.StatusCode)
	}
	if resp.ContentLength > 0 {
		fmt.Fprintf(os.Stderr, "Download size: %d MB\n", resp.ContentLength/(1000*1000))
	}
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
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
