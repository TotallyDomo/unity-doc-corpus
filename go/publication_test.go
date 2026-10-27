package main

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func buildFixture(t *testing.T, root, word string) (string, string) {
	t.Helper()
	source, output := filepath.Join(root, "source"), filepath.Join(root, "out")
	for _, section := range sectionDirs {
		writeExtractedPage(t, source, section+"/Example.html", `<title>Example</title><div id="content-wrap"><p>`+word+`</p></div>`)
	}
	writeTestZip(t, filepath.Join(source, "6000.3-UnityDocumentation.zip"), map[string]string{"Manual/Example.html": "retained", "ScriptReference/Example.html": "retained"})
	writeFetchMarker(t, source, "6000.3-UnityDocumentation.zip")
	if _, ok := readFetchInfo(source); !ok {
		t.Fatal("invalid source fixture marker")
	}
	if err := build(source, output, 2, true); err != nil {
		t.Fatal(err)
	}
	return source, output
}

func corpusBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = sha256Hex(data)
	}
	return out
}

func assertReadableGeneration(t *testing.T, output, word string) {
	t.Helper()
	md, err := pageMarkdown(output, "Manual/Example")
	if err != nil || !strings.Contains(md, word) {
		t.Fatalf("page generation %s: %q, %v", word, md, err)
	}
	results, err := searchCorpus(output, word, 10)
	if err != nil || len(results) != 2 {
		t.Fatalf("search generation %s: %v, %v", word, results, err)
	}
}

type failingTSV struct {
	io.WriteCloser
	writeErr, closeErr bool
	writes             *int
}

func (f failingTSV) Write(data []byte) (int, error) {
	*f.writes++
	if f.writeErr {
		return 0, errors.New("injected TSV write")
	}
	return f.WriteCloser.Write(data)
}

func (f failingTSV) Close() error {
	err := f.WriteCloser.Close()
	if f.closeErr {
		return errors.Join(err, errors.New("injected TSV close"))
	}
	return err
}

func TestBuildFailurePreservesCompleteCorpus(t *testing.T) {
	for _, failure := range []string{"reader", "sqlite-schema", "sqlite-insert", "sqlite-commit", "sqlite-close", "tsv-write", "tsv-flush", "tsv-close", "index.md", "manifest.json"} {
		t.Run(failure, func(t *testing.T) {
			source, output := buildFixture(t, t.TempDir(), "oldwombat")
			before := corpusBytes(t, output)
			title := "Example"
			if failure == "tsv-write" {
				title = strings.Repeat("x", 8192)
			}
			writeExtractedPage(t, source, "Manual/Example.html", `<title>`+title+`</title><div id="content-wrap">newquokka</div>`)
			deps := defaultBuildIO()
			calls := 0
			want := "injected"
			switch failure {
			case "reader":
				deps.readFile = func(string) ([]byte, error) { return nil, errors.New("injected read") }
			case "sqlite-schema":
				deps.createSQLite = func(path string) (*sql.DB, bool, error) {
					if err := os.Mkdir(path, 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(path, "occupied"), nil, 0644); err != nil {
						t.Fatal(err)
					}
					return createSQLite(path)
				}
				want = "docs.sqlite"
			case "sqlite-insert", "sqlite-commit":
				deps.createSQLite = func(path string) (*sql.DB, bool, error) {
					db, fts, err := createSQLite(path)
					if err != nil {
						return db, fts, err
					}
					statement := `CREATE TRIGGER fail_insert BEFORE INSERT ON pages BEGIN SELECT RAISE(ABORT, 'injected insert'); END`
					if failure == "sqlite-commit" {
						statement = `PRAGMA foreign_keys=ON; CREATE TABLE parent(id PRIMARY KEY); CREATE TABLE child(id REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_commit AFTER INSERT ON pages BEGIN INSERT INTO child VALUES (1); END`
					}
					if _, err := db.Exec(statement); err != nil {
						db.Close()
						return nil, false, err
					}
					return db, fts, nil
				}
				if failure == "sqlite-commit" {
					want = "FOREIGN KEY"
				}
			case "sqlite-close":
				deps.closeSQLite = func(db *sql.DB) error { return errors.Join(db.Close(), errors.New("injected SQLite close")) }
			case "tsv-write", "tsv-flush", "tsv-close":
				deps.createTSV = func(path string) (io.WriteCloser, error) {
					f, err := os.Create(path)
					return failingTSV{f, failure != "tsv-close", failure == "tsv-close", &calls}, err
				}
				if failure == "tsv-flush" {
					want = "flush TSV: injected"
				}
			case "index.md", "manifest.json":
				deps.writeFile = func(path string, data []byte, mode os.FileMode) error {
					if filepath.Base(path) == failure {
						return errors.New("injected metadata write")
					}
					return os.WriteFile(path, data, mode)
				}
			}
			err := buildWithIO(source, output, 2, false, deps)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: expected %q failure, got %v", failure, want, err)
			}
			if strings.HasPrefix(failure, "tsv-") && calls == 0 {
				t.Fatal("TSV fault not exercised")
			}
			if got := corpusBytes(t, output); !reflect.DeepEqual(before, got) {
				t.Fatal("failed build changed last-good bytes")
			}
			assertReadableGeneration(t, output, "oldwombat")
			if !sectionsPresent(source) {
				t.Fatal("failed build pruned recovery source")
			}
			for _, path := range []string{output + ".staging", output + ".previous"} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("failed build left %s: %v", path, err)
				}
			}
		})
	}
}

func TestPublicationInterruptedProcess(t *testing.T) {
	if phase := os.Getenv("UNITY_PUBLICATION_TEST_PHASE"); phase != "" {
		output, candidate := os.Getenv("UNITY_PUBLICATION_TEST_OUTPUT"), os.Getenv("UNITY_PUBLICATION_TEST_CANDIDATE")
		p, err := beginPublication(output)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(candidate)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Name() == corpusMarker {
				continue
			}
			data, err := os.ReadFile(filepath.Join(candidate, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p.stage, entry.Name()), data, 0644); err != nil {
				t.Fatal(err)
			}
		}
		if phase == "staged" {
			os.Exit(23)
		}
		p.rename = func(from, to string) error {
			if phase == "sealed" {
				os.Exit(23)
			}
			if err := os.Rename(from, to); err != nil {
				return err
			}
			if (phase == "parked" && from == p.output) || (phase == "installed" && from == p.stage) {
				os.Exit(23)
			}
			return nil
		}
		if err := p.publish(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("interruption not reached")
	}
	for _, phase := range []string{"staged", "sealed", "parked", "installed"} {
		t.Run(phase, func(t *testing.T) {
			_, output := buildFixture(t, filepath.Join(t.TempDir(), "old"), "oldwombat")
			_, candidate := buildFixture(t, filepath.Join(t.TempDir(), "new"), "newquokka")
			wantPath, word := output, "oldwombat"
			if phase == "installed" {
				wantPath, word = candidate, "newquokka"
			}
			want := corpusBytes(t, wantPath)
			want[corpusMarker] = sha256Hex([]byte(completeMarker + output + "\n"))
			cmd := exec.Command(os.Args[0], "-test.run=^TestPublicationInterruptedProcess$")
			cmd.Env = append(os.Environ(), "UNITY_PUBLICATION_TEST_PHASE="+phase, "UNITY_PUBLICATION_TEST_OUTPUT="+output, "UNITY_PUBLICATION_TEST_CANDIDATE="+candidate)
			data, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("child: %s, %v", data, err)
			}
			p, err := beginPublication(output)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.close(); err != nil {
				t.Fatal(err)
			}
			if got := corpusBytes(t, output); !reflect.DeepEqual(want, got) {
				t.Fatal("recovery mixed or changed generations")
			}
			assertReadableGeneration(t, output, word)
		})
	}
}

func TestPublicationRecoveryRefusesForeignAndAmbiguousState(t *testing.T) {
	for _, state := range []string{"foreign-stage", "ambiguous-output"} {
		t.Run(state, func(t *testing.T) {
			_, output := buildFixture(t, t.TempDir(), "oldwombat")
			path := output + ".staging"
			marker := completeMarker + output + ".staging\n"
			if state == "ambiguous-output" {
				path = output + ".previous"
				marker = "legacy marker"
				if err := os.WriteFile(filepath.Join(output, corpusMarker), []byte(marker), 0644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(path, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, corpusMarker), []byte(marker), 0644); err != nil {
				t.Fatal(err)
			}
			before, sibling := corpusBytes(t, output), corpusBytes(t, path)
			if p, err := beginPublication(output); err == nil {
				p.close()
				t.Fatal("ambiguous recovery accepted")
			}
			if !reflect.DeepEqual(before, corpusBytes(t, output)) || !reflect.DeepEqual(sibling, corpusBytes(t, path)) {
				t.Fatal("refusal changed preserved paths")
			}
		})
	}
}

func TestPublicationRenameFailuresAndLock(t *testing.T) {
	for _, phase := range []string{"park", "install", "rollback"} {
		t.Run(phase, func(t *testing.T) {
			_, output := buildFixture(t, t.TempDir(), "oldwombat")
			before := corpusBytes(t, output)
			p, err := beginPublication(output)
			if err != nil {
				t.Fatal(err)
			}
			if other, err := beginPublication(output); err == nil {
				other.close()
				t.Fatal("concurrent writer admitted")
			}
			sibling, err := beginPublication(filepath.Join(filepath.Dir(output), "sibling"))
			if err != nil {
				t.Fatal(err)
			}
			sibling.close()
			p.rename = func(from, to string) error {
				if (phase == "park" && from == p.output) || (phase != "park" && from == p.stage) || (phase == "rollback" && from == p.previous) {
					return errors.New("injected rename")
				}
				return os.Rename(from, to)
			}
			if err := p.publish(); err == nil || !strings.Contains(err.Error(), "injected rename") {
				t.Fatalf("expected rename failure: %v", err)
			}
			if err := p.close(); err != nil {
				t.Fatal(err)
			}
			if phase == "rollback" {
				assertReadableGeneration(t, output+".previous", "oldwombat")
			} else {
				assertReadableGeneration(t, output, "oldwombat")
			}
			recovered, err := beginPublication(output)
			if err != nil {
				t.Fatal(err)
			}
			recovered.close()
			if !reflect.DeepEqual(before, corpusBytes(t, output)) {
				t.Fatal("rename failure lost last-good bytes")
			}
		})
	}
}

func TestBuildPublicationSuccessAndPathGuards(t *testing.T) {
	source, output := buildFixture(t, t.TempDir(), "oldwombat")
	for _, section := range sectionDirs {
		writeExtractedPage(t, source, section+"/Example.html", `<title>Example</title><div id="content-wrap">newquokka</div>`)
	}
	if err := build(source, output, 2, false); err != nil {
		t.Fatal(err)
	}
	assertReadableGeneration(t, output, "newquokka")
	if sectionsPresent(source) {
		t.Fatal("successful build did not prune recoverable source")
	}
	for _, bad := range []string{source, filepath.Dir(source), filepath.Join(source, "Manual", "out"), filepath.Join(source, "ScriptReference")} {
		if err := build(source, bad, 1, false); err == nil {
			t.Fatalf("unsafe output accepted: %s", bad)
		}
	}
	for _, suffix := range []string{".staging", ".previous"} {
		path := output + suffix
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
		precious := filepath.Join(path, "precious")
		if err := os.WriteFile(precious, []byte("keep"), 0644); err != nil {
			t.Fatal(err)
		}
		if p, err := beginPublication(output); err == nil {
			p.close()
			t.Fatal("unmarked sibling accepted")
		}
		if data, err := os.ReadFile(precious); err != nil || !bytes.Equal(data, []byte("keep")) {
			t.Fatal("unmarked sibling changed")
		}
		if err := os.Remove(precious); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}
