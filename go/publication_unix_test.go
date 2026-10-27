//go:build !windows

package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPublicationPOSIXOpenHandle(t *testing.T) {
	_, output := buildFixture(t, t.TempDir(), "oldwombat")
	before := corpusBytes(t, output)
	path := filepath.Join(output, "index.md")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := beginPublication(output)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	if err := os.WriteFile(filepath.Join(p.stage, "index.md"), []byte("new generation"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := p.publish(); err != nil {
		// WSL's Windows-backed mounts can refuse renames while a reader is open.
		if !reflect.DeepEqual(before, corpusBytes(t, output)) {
			t.Fatal("open-reader refusal changed last-good bytes")
		}
		t.Logf("filesystem refused publication with an open reader, preserving old bytes: %v", err)
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := p.publish(); err != nil {
			t.Fatalf("publication after closing reader: %v", err)
		}
	} else {
		old, err := io.ReadAll(f)
		if err != nil || string(old) != string(want) {
			t.Fatalf("open old handle: %q, %v", old, err)
		}
	}
	newData, err := os.ReadFile(path)
	if err != nil || string(newData) != "new generation" {
		t.Fatalf("new path: %q, %v", newData, err)
	}
}
