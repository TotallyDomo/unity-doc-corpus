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

// unzip -> findDocRoot -> copyTree is the fetch extraction contract: the zip expands intact, the
// documentation root (the dir holding both Manual and ScriptReference) is located even when
// nested under a top-level folder, and copyTree reproduces the tree faithfully.
func TestUnzipFindDocRootCopyTree(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "docs.zip")
	writeTestZip(t, zipPath, map[string]string{
		"UnityDocs/Documentation/en/Manual/Intro.html":        "<html>manual</html>",
		"UnityDocs/Documentation/en/ScriptReference/Foo.html": "<html>ref</html>",
		"UnityDocs/Documentation/en/uploads/asset.bin":        "binary",
	})

	extract := filepath.Join(tmp, "extract")
	if err := unzip(zipPath, extract); err != nil {
		t.Fatalf("unzip: %v", err)
	}

	docRoot, err := findDocRoot(extract)
	if err != nil {
		t.Fatalf("findDocRoot: %v", err)
	}
	if filepath.Base(docRoot) != "en" {
		t.Errorf("docRoot = %q, want the nested .../en directory", docRoot)
	}

	dest := filepath.Join(tmp, "dest")
	if err := copyTree(docRoot, dest); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	for _, rel := range []string{"Manual/Intro.html", "ScriptReference/Foo.html", "uploads/asset.bin"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Errorf("copied tree missing %s: %v", rel, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dest, "Manual", "Intro.html"))
	if err != nil || string(got) != "<html>manual</html>" {
		t.Errorf("copied content mismatch: %q, %v", got, err)
	}
}

// unzip must reject zip-slip entries that resolve outside the destination rather than writing
// through the traversal - the guard that stops a hostile archive from escaping the extract dir.
func TestUnzipRejectsZipSlip(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "evil.zip")
	writeTestZip(t, zipPath, map[string]string{
		"../escape.txt": "pwned",
	})
	extract := filepath.Join(tmp, "extract")
	if err := unzip(zipPath, extract); err == nil {
		t.Fatal("expected unzip to reject a path-traversal entry")
	}
	if _, err := os.Stat(filepath.Join(tmp, "escape.txt")); err == nil {
		t.Fatal("zip-slip guard failed: file written outside destination")
	}
}

// findDocRoot fails clearly when no Manual+ScriptReference pair exists anywhere in the tree.
func TestFindDocRootMissing(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "Manual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := findDocRoot(tmp); err == nil {
		t.Fatal("expected error when ScriptReference is absent")
	}
}
