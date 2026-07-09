package main

import (
	"archive/zip"
	"os"
	"path/filepath"
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
