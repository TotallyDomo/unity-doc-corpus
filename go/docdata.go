package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func pageIDFor(section, path, root string) (string, error) {

	rel, err := filepath.Rel(filepath.Join(root, section), path)

	if err != nil {

		return "", err

	}

	rel = filepath.ToSlash(rel)

	return strings.TrimSuffix(rel, ".html"), nil

}

func collectHTML(root string) ([]string, error) {

	var files []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {

		if err != nil {

			return err

		}

		if !d.IsDir() && strings.EqualFold(filepath.Ext(path), ".html") {

			files = append(files, path)

		}

		return nil

	})

	sort.Slice(files, func(i, j int) bool { return filepath.ToSlash(files[i]) < filepath.ToSlash(files[j]) })

	return files, err

}

func loadDocdataTitles(root, section string) map[string]string {

	titles := map[string]string{}

	data, err := os.ReadFile(filepath.Join(root, section, "docdata", "index.json"))

	if err != nil {

		return titles

	}

	var parsed struct {
		Pages [][]any `json:"pages"`
	}

	if json.Unmarshal(data, &parsed) != nil {

		return titles

	}

	for _, item := range parsed.Pages {

		if len(item) >= 2 {

			id, ok1 := item[0].(string)

			title, ok2 := item[1].(string)

			if ok1 && ok2 {

				titles[id] = title

			}

		}

	}

	return titles

}

func writeMarkdown(rec record, links []link) []byte {

	var b strings.Builder

	b.WriteString("---\n")

	fmt.Fprintf(&b, "section: %s\n", rec.Section)

	fmt.Fprintf(&b, "page_id: %s\n", strings.Trim(jsonString(rec.PageID), `"`))

	fmt.Fprintf(&b, "title: %s\n", strings.Trim(jsonString(rec.Title), `"`))

	fmt.Fprintf(&b, "source_rel: %s\n", rec.SourceRel)

	fmt.Fprintf(&b, "source_sha256: %s\n", rec.SourceSHA256)

	fmt.Fprintf(&b, "canonical_url: %s\n", rec.CanonicalURL)

	b.WriteString("---\n\n")

	title := rec.Title

	if title == "" {

		title = rec.PageID

	}

	fmt.Fprintf(&b, "# %s\n\n", title)

	fmt.Fprintf(&b, "Source: `%s`\n", rec.SourceRel)

	if rec.CanonicalURL != "" {

		fmt.Fprintf(&b, "Canonical: %s\n", rec.CanonicalURL)

	}

	if rec.UnityVersion != "" {

		fmt.Fprintf(&b, "Version: %s\n", rec.UnityVersion)

	}

	body := rec.Body

	if body == "" {

		body = "[No extracted content]"

	}

	b.WriteString("\n## Content\n\n")

	b.WriteString(body)

	if len(links) > 0 {

		b.WriteString("\n\n## Content Links")

		seen := map[link]bool{}

		for _, l := range links {

			if !seen[l] {

				seen[l] = true

				fmt.Fprintf(&b, "\n- %s -> %s", l.Text, l.Href)

			}

		}

	}

	return []byte(strings.TrimRight(b.String(), " \n") + "\n")

}

func jsonString(value string) string {

	data, _ := json.Marshal(value)

	return string(data)

}
