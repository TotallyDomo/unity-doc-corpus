package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestZip builds a zip at path from a name->content map. Directory entries are implied by
// file paths; a trailing "/" name creates an explicit directory entry.
func writeTestZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	for name, content := range entries {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// extractSections is the fetch extraction contract: only Manual and ScriptReference (with their
// nested docdata) are pulled from the zip, the doc-root prefix is stripped so they land at the
// top of dest, and unused siblings (uploads/static assets) are left behind.
func TestExtractSectionsSelective(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "docs.zip")
	writeTestZip(t, zipPath, map[string]string{
		"UnityDocs/Documentation/en/Manual/Intro.html":         "<html>manual</html>",
		"UnityDocs/Documentation/en/Manual/docdata/index.json": `{"pages":[["Intro","Introduction"]]}`,
		"UnityDocs/Documentation/en/ScriptReference/Foo.html":  "<html>ref</html>",
		"UnityDocs/Documentation/en/uploads/asset.bin":         "binary",
		"UnityDocs/Documentation/en/StaticFiles/style.css":     "body{}",
	})

	dest := filepath.Join(tmp, "dest")
	if err := extractSections(zipPath, dest, 4); err != nil {
		t.Fatalf("extractSections: %v", err)
	}

	// Wanted subtrees are present, prefix-stripped; nested docdata (titles source) survives.
	for _, rel := range []string{"Manual/Intro.html", "Manual/docdata/index.json", "ScriptReference/Foo.html"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Errorf("expected %s to be extracted: %v", rel, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dest, "Manual", "Intro.html"))
	if err != nil || string(got) != "<html>manual</html>" {
		t.Errorf("extracted content mismatch: %q, %v", got, err)
	}
	// Unused siblings are skipped entirely.
	for _, rel := range []string{"uploads/asset.bin", "StaticFiles/style.css"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("expected %s to be skipped, but it exists (err=%v)", rel, err)
		}
	}
}

// A zip-root layout (Manual/ and ScriptReference/ at the top) yields an empty prefix and still
// extracts correctly. Single worker exercises the serial path.
func TestExtractSectionsRootLayout(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "docs.zip")
	writeTestZip(t, zipPath, map[string]string{
		"Manual/A.html":          "a",
		"ScriptReference/B.html": "b",
		"uploads/c.bin":          "c",
	})
	dest := filepath.Join(tmp, "dest")
	if err := extractSections(zipPath, dest, 1); err != nil {
		t.Fatalf("extractSections: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "Manual", "A.html")); err != nil {
		t.Errorf("Manual/A.html missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "uploads", "c.bin")); !os.IsNotExist(err) {
		t.Errorf("uploads should be skipped")
	}
}

// extractSections must reject zip-slip entries that resolve outside the destination rather than
// writing through the traversal.
func TestExtractSectionsRejectsZipSlip(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "evil.zip")
	writeTestZip(t, zipPath, map[string]string{
		"Manual/ok.html":          "ok",
		"ScriptReference/ok.html": "ok",
		"Manual/../../escape.txt": "pwned",
	})
	dest := filepath.Join(tmp, "dest")
	if err := extractSections(zipPath, dest, 2); err == nil {
		t.Fatal("expected extractSections to reject a path-traversal entry")
	}
	if _, err := os.Stat(filepath.Join(tmp, "escape.txt")); err == nil {
		t.Fatal("zip-slip guard failed: file written outside destination")
	}
}

func TestExtractSectionsRejectsDuplicateTargets(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "duplicate.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, entry := range []struct{ name, body string }{
		{"Manual/A.html", "first"},
		{"Manual/A.html", "second"},
		{"ScriptReference/B.html", "ref"},
	} {
		fw, err := w.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	err = extractSections(zipPath, filepath.Join(tmp, "dest"), 4)
	if err == nil || !strings.Contains(err.Error(), "same output path") {
		t.Fatalf("duplicate zip targets must be refused deterministically, got: %v", err)
	}
}

// findSectionPrefix / extractSections fail clearly when the zip lacks a directory holding both
// Manual and ScriptReference.
func TestExtractSectionsMissingSection(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "docs.zip")
	writeTestZip(t, zipPath, map[string]string{
		"UnityDocs/en/Manual/Intro.html": "<html>manual</html>",
	})
	dest := filepath.Join(tmp, "dest")
	if err := extractSections(zipPath, dest, 2); err == nil {
		t.Fatal("expected error when ScriptReference is absent")
	}
}

// findSectionPrefix picks the shortest prefix that holds all sections, even when the section
// names also appear deeper in the tree.
func TestFindSectionPrefix(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "docs.zip")
	writeTestZip(t, zipPath, map[string]string{
		"root/Manual/Intro.html":                    "m",
		"root/ScriptReference/Foo.html":             "s",
		"root/Manual/ScriptReference/nested/x.html": "nested-noise",
	})
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	prefix, err := findSectionPrefix(zr.File)
	if err != nil {
		t.Fatalf("findSectionPrefix: %v", err)
	}
	if prefix != "root/" {
		t.Errorf("prefix = %q, want %q", prefix, "root/")
	}
}

func TestZipURLReMatchesBothBucketHosts(t *testing.T) {
	matches := map[string]string{
		`<a href="https://cloudmedia-docs.unity3d.com/docscloudstorage/en/6000.3/UnityDocumentation.zip">`: "https://cloudmedia-docs.unity3d.com/docscloudstorage/en/6000.3/UnityDocumentation.zip",
		`<a href="https://storage.googleapis.com/docscloudstorage/2019.4/UnityDocumentation.zip">`:         "https://storage.googleapis.com/docscloudstorage/2019.4/UnityDocumentation.zip",
	}
	for page, want := range matches {
		if got := zipURLRe.FindString(page); got != want {
			t.Fatalf("zipURLRe on %q = %q, want %q", page, got, want)
		}
	}
	rejected := []string{
		`<a href="https://evil.example.com/docscloudstorage/en/6000.3/UnityDocumentation.zip">`,
		`<a href="https://storage.googleapis.com/otherbucket/2019.4/UnityDocumentation.zip">`,
	}
	for _, page := range rejected {
		if got := zipURLRe.FindString(page); got != "" {
			t.Fatalf("zipURLRe matched untrusted URL %q in %q", got, page)
		}
	}
}

func TestValidateFetchInfoRejectsUnsafeMarkerFields(t *testing.T) {
	valid := fetchInfo{
		UnityVersion: "6000.3",
		ZipURL:       "https://cloudmedia-docs.unity3d.com/docscloudstorage/en/6000.3/UnityDocumentation.zip",
		ZipSHA256:    strings.Repeat("a", 64),
		FetchedAtUTC: "2026-07-12T00:00:00Z",
		ZipName:      "6000.3-UnityDocumentation.zip",
	}
	if err := validateFetchInfo(valid); err != nil {
		t.Fatalf("valid marker rejected: %v", err)
	}
	for name, mutate := range map[string]func(*fetchInfo){
		"version traversal": func(i *fetchInfo) { i.UnityVersion = "../6000.3" },
		"zip traversal":     func(i *fetchInfo) { i.ZipName = "../outside.zip" },
		"foreign bucket":    func(i *fetchInfo) { i.ZipURL = "https://storage.googleapis.com/other/x.zip" },
		"bad hash":          func(i *fetchInfo) { i.ZipSHA256 = "x" },
		"bad time":          func(i *fetchInfo) { i.FetchedAtUTC = "today" },
	} {
		got := valid
		mutate(&got)
		if err := validateFetchInfo(got); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

func TestSalvageRetainedZipVerifiesMarkerHash(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "docs")
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	zipName := "6000.3-UnityDocumentation.zip"
	retained := filepath.Join(dest, zipName)
	writeTestZip(t, retained, map[string]string{
		"Manual/A.html":          "a",
		"ScriptReference/B.html": "b",
	})
	actualSHA, err := hashFileSHA256(retained)
	if err != nil {
		t.Fatal(err)
	}
	info := fetchInfo{
		UnityVersion: "6000.3",
		ZipURL:       "https://cloudmedia-docs.unity3d.com/docscloudstorage/en/6000.3/UnityDocumentation.zip",
		ZipSHA256:    strings.Repeat("0", 64),
		FetchedAtUTC: "2026-07-12T00:00:00Z",
		ZipName:      zipName,
	}
	writeMarker := func() {
		data, err := json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, fetchMarkerName), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeMarker()
	cacheZip := filepath.Join(cache, zipName)
	if err := salvageRetainedZip(dest, cacheZip, "6000.3", info.ZipURL); err == nil {
		t.Fatal("marker/hash mismatch must refuse retained-zip reuse")
	}
	if _, err := os.Stat(retained); err != nil {
		t.Fatalf("refused salvage must leave retained zip untouched: %v", err)
	}
	info.ZipSHA256 = actualSHA
	writeMarker()
	resolvedURL := "https://storage.googleapis.com/docscloudstorage/6000.3/UnityDocumentation.zip"
	if err := salvageRetainedZip(dest, cacheZip, "6000.3", resolvedURL); err != nil {
		t.Fatalf("valid retained zip refused: %v", err)
	}
	if got, err := hashFileSHA256(cacheZip); err != nil || got != actualSHA {
		t.Fatalf("salvaged zip mismatch: sha=%s err=%v", got, err)
	}
	if got, err := validateCachedZip(cacheZip, resolvedURL); err != nil || got != actualSHA {
		t.Fatalf("salvaged zip provenance mismatch: sha=%s err=%v", got, err)
	}
	if _, err := os.Stat(retained); !os.IsNotExist(err) {
		t.Fatal("successful salvage must move the retained zip into cache")
	}
}

func TestCachedZipRequiresTrustedProvenance(t *testing.T) {
	root := t.TempDir()
	zipPath := filepath.Join(root, "6000.3-UnityDocumentation.zip")
	writeTestZip(t, zipPath, map[string]string{
		"Manual/A.html":          "a",
		"ScriptReference/B.html": "b",
	})
	url := "https://cloudmedia-docs.unity3d.com/docscloudstorage/en/6000.3/UnityDocumentation.zip"
	if _, err := validateCachedZip(zipPath, url); err == nil {
		t.Fatal("a pre-positioned cache zip without provenance must not be trusted")
	}
	sha, err := hashFileSHA256(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCacheInfo(zipPath, url, sha); err != nil {
		t.Fatal(err)
	}
	if got, err := validateCachedZip(zipPath, url); err != nil || got != sha {
		t.Fatalf("valid cache provenance rejected: sha=%s err=%v", got, err)
	}
	if _, err := validateCachedZip(zipPath, strings.Replace(url, "6000.3", "6000.4", 1)); err == nil {
		t.Fatal("cache provenance for another resolved URL must be refused")
	}
}
