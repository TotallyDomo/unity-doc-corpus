package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// defaultFetchDestination is a portable, relative docs root under the current working
// directory. It is deliberately not an absolute machine path so the tool stays project- and
// workstation-independent; pass --destination to place the docs anywhere.
const defaultFetchDestination = "unity-docs"

var httpClient = &http.Client{Timeout: 30 * time.Minute}

var zipURLRe = regexp.MustCompile(`https://cloudmedia-docs\.unity3d\.com/docscloudstorage/en/[^"]+/UnityDocumentation\.zip`)

func runFetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	version := fs.String("version", "", "Unity major.minor documentation stream, e.g. 6000.3.")
	destination := fs.String("destination", defaultFetchDestination, "Directory to populate with Manual/ScriptReference docs.")
	cacheRoot := fs.String("cache-root", "", "Download/extract cache dir. Defaults to <os-temp-dir>/unity-doc-downloads.")
	force := fs.Bool("force", false, "Replace an existing destination directory.")
	resolveOnly := fs.Bool("resolve-only", false, "Print the resolved zip URL and exit without downloading.")
	_ = fs.Parse(args)
	if *version == "" {
		fmt.Fprintln(os.Stderr, "Usage: unity-doc-corpus fetch --version <ver> [--destination <dir>] [--cache-root <dir>] [--force] [--resolve-only]")
		os.Exit(2)
	}
	if err := fetch(*version, *destination, *cacheRoot, *force, *resolveOnly); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func fetch(version, destination, cacheRoot string, force, resolveOnly bool) error {
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
	extractPath := filepath.Join(cacheAbs, version+"-expanded")

	if _, err := os.Stat(zipPath); err != nil {
		fmt.Fprintf(os.Stderr, "Downloading about 300MB to %s\n", zipPath)
		if err := downloadFile(zipURL, zipPath); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, "Using cached zip:", zipPath)
	}

	if err := os.RemoveAll(extractPath); err != nil {
		return err
	}
	if err := os.MkdirAll(extractPath, 0o755); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Extracting zip to", extractPath)
	if err := unzip(zipPath, extractPath); err != nil {
		return err
	}

	docRoot, err := findDocRoot(extractPath)
	if err != nil {
		return err
	}
	if force {
		if err := os.RemoveAll(destAbs); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(destAbs, 0o755); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Copying documentation files to", destAbs)
	if err := copyTree(docRoot, destAbs); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Done. Source root for the corpus builder:", destAbs)
	fmt.Println(destAbs)
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

func unzip(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for _, f := range r.File {
		target := filepath.Join(destAbs, f.Name)
		// Zip-slip guard: reject entries that resolve outside the destination.
		if target != destAbs && !strings.HasPrefix(target, destAbs+string(os.PathSeparator)) {
			return fmt.Errorf("zip entry escapes destination: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeZipEntry(f, target); err != nil {
			return err
		}
	}
	return nil
}

func writeZipEntry(f *zip.File, target string) error {
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

// findDocRoot returns the first directory under root that contains both Manual and
// ScriptReference subdirectories - the documentation root the corpus builder expects.
func findDocRoot(root string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if hasDir(filepath.Join(path, "Manual")) && hasDir(filepath.Join(path, "ScriptReference")) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("could not find extracted documentation root containing Manual and ScriptReference under %s", root)
	}
	return found, nil
}

func hasDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
