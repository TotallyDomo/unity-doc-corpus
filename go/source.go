package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func runSource(args []string) {
	fs := flag.NewFlagSet("source", flag.ExitOnError)
	source := fs.String("source", defaultFetchDestination, "Unity documentation root (holds the extracted docs and/or the retained zip).")
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "Usage: unity-doc-corpus source [--source <docs-root>] <source_rel>")
		fmt.Fprintln(os.Stderr, "  <source_rel> as recorded in a page's frontmatter, e.g. Manual/android-export-process.html")
		os.Exit(2)
	}
	origin, err := writeSourceHTML(*source, fs.Arg(0), os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "Source:", origin)
}

// writeSourceHTML streams the original HTML for a page to w, preferring the extracted tree
// and falling back to the retained offline zip named by the fetch marker. rel is the page's
// source_rel exactly as the corpus frontmatter records it.
func writeSourceHTML(root, rel string, w io.Writer) (string, error) {
	rel = filepath.ToSlash(rel)
	if strings.Contains(rel, "..") || strings.HasPrefix(rel, "/") || !underSection(rel) {
		return "", fmt.Errorf("not a valid page source_rel (want e.g. Manual/<page>.html): %s", rel)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}

	extracted := filepath.Join(rootAbs, filepath.FromSlash(rel))
	if f, err := os.Open(extracted); err == nil {
		defer f.Close()
		if _, err := io.Copy(w, f); err != nil {
			return "", err
		}
		return extracted, nil
	}

	zipPath, ok := retainedZipPath(rootAbs)
	if !ok {
		// Distinguish "the docs are not here at all" from "the docs are here but no such
		// page": with an extracted tree present, a failed open almost always means a
		// misspelled source_rel, and telling the user to re-fetch would misdiagnose it.
		if section, _, found := strings.Cut(rel, "/"); found {
			if info, err := os.Stat(filepath.Join(rootAbs, section)); err == nil && info.IsDir() {
				return "", fmt.Errorf("page %s not found in the extracted tree under %s (check the source_rel spelling against the page's frontmatter; no retained zip to fall back to)", rel, rootAbs)
			}
		}
		return "", fmt.Errorf("neither extracted HTML nor a retained zip under %s - re-run fetch, or read the page's canonical_url online", rootAbs)
	}
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", err
	}
	defer r.Close()
	prefix, err := findSectionPrefix(r.File)
	if err != nil {
		return "", err
	}
	member := prefix + rel
	for _, f := range r.File {
		if filepath.ToSlash(f.Name) != member {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		defer rc.Close()
		if _, err := io.Copy(w, rc); err != nil {
			return "", err
		}
		return zipPath + "!" + member, nil
	}
	return "", fmt.Errorf("page %s not found in %s", rel, zipPath)
}

// retainedZipPath locates the retained offline docs zip under root: the name the fetch
// marker records, with a glob fallback for markers that predate zip retention.
func retainedZipPath(rootAbs string) (string, bool) {
	if info, ok := readFetchInfo(rootAbs); ok && info.ZipName != "" {
		p := filepath.Join(rootAbs, info.ZipName)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	matches, _ := filepath.Glob(filepath.Join(rootAbs, "*-UnityDocumentation.zip"))
	if len(matches) == 1 {
		return matches[0], true
	}
	return "", false
}
