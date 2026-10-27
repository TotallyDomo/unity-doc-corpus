package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func TestPublicationWindowsOpenHandle(t *testing.T) {
	_, output := buildFixture(t, t.TempDir(), "oldwombat")
	before := corpusBytes(t, output)
	name, err := windows.UTF16PtrFromString(filepath.Join(output, "docs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	p, err := beginPublication(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.publish(); err == nil {
		t.Fatal("rename with a non-delete-sharing reader unexpectedly succeeded")
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, corpusBytes(t, output)) {
		t.Fatal("open-handle refusal changed last-good bytes")
	}
	if _, err := os.Stat(output + ".previous"); !os.IsNotExist(err) {
		t.Fatal("parked corpus after failed rename")
	}
}
