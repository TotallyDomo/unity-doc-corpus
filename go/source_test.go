package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFetchMarker(t *testing.T, root, zipName string) {
	t.Helper()
	marker := `{"unity_version":"6000.3","zip_url":"https://example","zip_sha256":"x","fetched_at_utc":"2026-07-09T00:00:00Z","zip_name":"` + zipName + `"}`
	if err := os.WriteFile(filepath.Join(root, fetchMarkerName), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExtractedPage(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSourceHTML prefers the extracted tree, falls back to the retained zip member, and
// rejects anything that is not a section-relative page path.
func TestWriteSourceHTMLExtractedThenZip(t *testing.T) {
	root := t.TempDir()
	writeTestZip(t, filepath.Join(root, "6000.3-UnityDocumentation.zip"), map[string]string{
		"Documentation/en/Manual/Foo.html":        "<html>from zip</html>",
		"Documentation/en/ScriptReference/A.html": "<html>ref</html>",
	})
	writeFetchMarker(t, root, "6000.3-UnityDocumentation.zip")
	writeExtractedPage(t, root, "Manual/Foo.html", "<html>from tree</html>")

	var out strings.Builder
	origin, err := writeSourceHTML(root, "Manual/Foo.html", &out)
	if err != nil || out.String() != "<html>from tree</html>" {
		t.Fatalf("extracted-tree read: origin=%q out=%q err=%v", origin, out.String(), err)
	}

	// Remove the extracted tree: the same page must now come out of the zip member.
	if err := os.RemoveAll(filepath.Join(root, "Manual")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	origin, err = writeSourceHTML(root, "Manual/Foo.html", &out)
	if err != nil || out.String() != "<html>from zip</html>" {
		t.Fatalf("zip fallback read: origin=%q out=%q err=%v", origin, out.String(), err)
	}
	if !strings.Contains(origin, ".zip!") {
		t.Errorf("zip origin should name the member, got %q", origin)
	}

	for _, bad := range []string{"../secrets.txt", "/etc/passwd", "Manual/../../x.html", "uploads/a.bin", "docs.sqlite"} {
		if _, err := writeSourceHTML(root, bad, &strings.Builder{}); err == nil {
			t.Errorf("path %q must be rejected", bad)
		}
	}
}

// pruneExtractedSource must delete the extracted sections only when BOTH the fetch marker
// and the retained zip are present - never a directory the tool cannot prove it created,
// and never the only remaining copy of the docs.
func TestPruneExtractedSourceGuards(t *testing.T) {
	sectionsExist := func(root string) bool {
		_, err := os.Stat(filepath.Join(root, "Manual", "Foo.html"))
		return err == nil
	}
	setup := func(marker, withZip bool) string {
		root := t.TempDir()
		writeExtractedPage(t, root, "Manual/Foo.html", "x")
		writeExtractedPage(t, root, "ScriptReference/A.html", "x")
		if marker {
			zipName := ""
			if withZip {
				zipName = "6000.3-UnityDocumentation.zip"
				writeTestZip(t, filepath.Join(root, zipName), map[string]string{"Manual/Foo.html": "x", "ScriptReference/A.html": "x"})
			}
			writeFetchMarker(t, root, zipName)
		}
		return root
	}

	root := setup(false, false)
	pruneExtractedSource(root)
	if !sectionsExist(root) {
		t.Error("no marker: sections must survive")
	}
	root = setup(true, false)
	pruneExtractedSource(root)
	if !sectionsExist(root) {
		t.Error("marker but no zip: sections must survive")
	}
	root = setup(true, true)
	pruneExtractedSource(root)
	if sectionsExist(root) {
		t.Error("marker + zip: sections must be pruned")
	}
	if _, err := os.Stat(filepath.Join(root, "6000.3-UnityDocumentation.zip")); err != nil {
		t.Error("prune must never touch the retained zip")
	}
	if _, err := os.Stat(filepath.Join(root, fetchMarkerName)); err != nil {
		t.Error("prune must keep the fetch marker")
	}
}

func TestMoveFileAcrossDirs(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "a", "f.txt")
	dst := filepath.Join(tmp, "b", "f.txt")
	for _, d := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source must be gone after move")
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "payload" {
		t.Errorf("moved content mismatch: %q %v", got, err)
	}
}
