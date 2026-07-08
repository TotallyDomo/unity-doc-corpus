# unity-doc-corpus

Turn Unity's official offline documentation into an agent-optimized local search corpus:
stripped Markdown, SQLite FTS5 full-text search, and a benchmarked lookup path - built
entirely on your machine by a pure-Go (CGO-free) tool.

No documentation content lives in this repository, and none ever has: you fetch Unity's
official offline docs zip yourself, and the builder derives the corpus locally. The repo
ships tooling and agent skills only.

## Numbers

Unity 6000.3 offline documentation, reference build (reproduce with the benchmarks below):

| Metric | Value |
| --- | --- |
| Pages transformed (Manual + ScriptReference) | 39,056 |
| Source HTML | 648 MB |
| Derived Markdown | 76 MB (11.7% of source bytes) |
| Full corpus build | under a minute wall clock (8 workers) |
| Recall benchmark | TBD - populated from `benchmark-report-expanded.json` |

Why this matters for agents: documentation lookups happen inside a context window billed per
token. An ~88% byte reduction per page - with a recorded source path and SHA-256 for every
transformed page - means cheap lookups that keep a verification path back to the original
HTML.

## How it works

1. `fetch` downloads Unity's official offline docs zip (only from `docs.unity3d.com` /
   `cloudmedia-docs.unity3d.com`) and unpacks it locally.
2. `build` walks the Manual and ScriptReference HTML and derives, per page: stripped
   Markdown (`text/`), metadata with source path and SHA-256 (`pages.jsonl`), a SQLite FTS5
   index (`docs.sqlite`), and an exact-name lookup table (`search_index.tsv`).
3. Agents query the derived corpus and verify load-bearing claims against the untouched
   originals.

The builder is Unity-specific; the pattern is not - a write-up on the generic
searchable-docs -> offline-fetch -> agent-transform -> router-skill shape is planned (see
`docs/DESIGN.md`).

## Quickstart

Requires Go 1.26+. Python 3 is optional (maintenance benchmarks only).

```
git clone https://github.com/TotallyDomo/unity-doc-corpus
cd unity-doc-corpus

# 1. Build the tools from source (no prebuilt binaries are shipped)
cd go
go build -trimpath -o ../bin/unity-doc-corpus .
go build -trimpath -o ../bin/unity-doc-corpus-benchmark ./cmd/benchmark
cd ..
# Windows: append .exe to the -o names, or run scripts/build.ps1

# 2. Fetch Unity's official offline docs (~300 MB, one-time per version)
bin/unity-doc-corpus fetch --version 6000.3

# 3. Build the derived corpus (writes unity-docs/_agent)
bin/unity-doc-corpus build --source unity-docs --output unity-docs/_agent

# 4. Try a lookup
sqlite3 unity-docs/_agent/docs.sqlite "SELECT p.title, p.md_rel FROM pages_fts f JOIN pages p ON p.page_key = f.page_key WHERE pages_fts MATCH 'addressables memory' ORDER BY bm25(pages_fts) LIMIT 5;"
```

## Agent skills

Two Claude Code skills live under `skills/`:

- **`unity-docs`** - lookup. Searches the built corpus for Unity Manual / Scripting API
  answers, with a verify-against-source step. This is the one you want day to day.
- **`unity-doc-corpus`** - builder/maintenance. Fetch, build, refresh, compare, benchmark.
  Expensive and explicitly triggered; never fires on ordinary doc questions.

Install with `npx skills add TotallyDomo/unity-doc-corpus` (both skills), or add
`--skill unity-docs` for lookup only. The corpus itself is still built once via the
Quickstart above.

## Trust surface

- **Network**: `fetch` talks only to Unity's official documentation hosts
  (`docs.unity3d.com` to resolve the zip URL, `cloudmedia-docs.unity3d.com` for the zip
  itself; both pinned in `go/fetch.go`). Nothing else fetches anything at runtime; the
  lookup skill is pure local reads.
- **Executes**: the two Go binaries you build from this repo's source, plus optional local
  Python scripts. No prebuilt binaries, no piped installers, no hooks.
- **Data egress**: none.

## Legal posture

Unity's documentation belongs to Unity. This repository never contains or redistributes it -
not in the tree, not in git history. You download the official offline zip from Unity
yourself; the derived corpus stays on your machine.

## Support

Built for my own workflow and shared as-is. PRs welcome; no support promised.

## License

MIT.
