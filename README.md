# unity-doc-corpus

[![tests](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/tests.yml/badge.svg)](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/tests.yml)
[![govulncheck](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/govulncheck.yml/badge.svg)](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/govulncheck.yml)
[![gitleaks](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/gitleaks.yml/badge.svg)](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/gitleaks.yml)

Turn Unity's official offline documentation into an agent-optimized local search corpus:
stripped Markdown, SQLite FTS5 full-text search, and a benchmarked lookup path - built
entirely on your machine by a pure-Go (CGO-free) tool.

No documentation content lives in this repository: you fetch Unity's
official offline docs zip yourself, and the builder derives the corpus locally. The repo
ships tooling and agent skills only.

## Numbers

Unity 6000.3 offline documentation, reference build (the checked-in reference run is
[docs/benchmark-6000.3.json](docs/benchmark-6000.3.json); reproduce it with the command
below):

| Metric | Value |
| --- | --- |
| Pages transformed (Manual + ScriptReference) | 39,056 |
| Source HTML | 648 MB |
| Derived Markdown | 62 MB (9.5% of source bytes) |
| Full corpus build | ~45 s right after fetch (8 workers); I/O-bound, budget 2-3x on a cold file cache |
| Top-10 recall, corpus FTS5 (title-weighted bm25) | 96.8% (976/1008 cases; 95.7% on Manual concept pages) |
| Top-10 recall, same bm25 over the raw HTML | 96.9% (977/1008) - recall parity with the corpus lane |
| Top-10 recall, naive ranked scan of the raw HTML (grep-style) | 93.8% (945/1008; 59.1% on Manual concept pages), ~50x slower (~207 ms vs ~4.2 ms per query) |

Benchmark cases are 8 curated lookups plus 1000 generated from page titles and page ids,
sampled evenly across the whole corpus so the mix matches its ~91% ScriptReference / ~9%
Manual composition; a case counts as recalled when the expected page appears in the top 10
results. Reproduce with
`bin/unity-doc-corpus-benchmark --source unity-docs --corpus unity-docs/_agent --generated-cases 1000`.

Retrieval is only half the guarantee - the other half is that the transform loses no
page text. That is checked mechanically, not assumed: after every transform change,
`bin/unity-doc-corpus audit --source unity-docs --corpus unity-docs/_agent --baseline docs/audit-baseline-6000.3.json --shared-baseline docs/shared-content-baseline-6000.3.json`
re-extracts every page's visible text with an extractor that shares no code with the
production parser and fails if page-unique content is missing from the derived Markdown,
if a page's derived/reference size ratio collapses, or if the corpus lists fewer pages
than the source tree holds. It needs the extracted HTML on disk (build with
`--keep-source`) and covers the full corpus in a few seconds. The checked-in baseline
([docs/audit-baseline-6000.3.json](docs/audit-baseline-6000.3.json)) lists the 496
individually triaged false positives (1.27% of pages, one known footer-adjacency class)
with their accepted magnitudes pinned, so the audit gates on new flags and on any
worsening of an accepted one. The `--shared-baseline` manifest
([docs/shared-content-baseline-6000.3.json](docs/shared-content-baseline-6000.3.json))
extends the guard to shared boilerplate sentences repeated across many pages (e.g. the
`hideFlags` description on 327 pages), which the page-local check alone cannot see - a
class-wide strip of such a sentence gates the run. The audit is a strong regression
detector, not a mathematical proof - it works at word-token granularity and has
documented false-negative classes (punctuation-only changes, sub-run stream edges); the
precise statement of what it does and does not prove is in
[docs/DESIGN.md](docs/DESIGN.md).

Why this matters for agents: documentation lookups happen inside a context window billed
per token. This corpus does not claim better retrieval than indexing Unity's raw HTML -
measured with the same ranker, recall matches to within one case in a thousand - and that
parity is the point: you keep the
recall while every page an agent actually reads shrinks by ~90%, and the search index
shrinks ~10x (86 MB vs ~860 MB for the same recall over raw HTML). The transform is
deliberately lossy (tables flatten, code loses fencing), so every derived page records the
source path and SHA-256 of the original it came from; when an answer hinges on one page's
exact details, the untouched original is one local command away
(`bin/unity-doc-corpus source <source_rel>`). That is insurance against transform bugs,
not a routine second read - the corpus-wide audit above is what keeps it that way.

## How it works

1. `fetch` downloads Unity's official offline docs zip (only from `docs.unity3d.com` and
   Unity's `docscloudstorage` bucket) and extracts just the Manual and Scripting API
   reference from it - the parts the corpus is built from - in parallel, straight to disk.
   Any stream Unity publishes offline docs for works; zips exist back to at least 5.6.
2. `build` walks the Manual and ScriptReference HTML and derives, per page: stripped
   Markdown (`text/`), metadata with source path and SHA-256 (`pages.jsonl`), a SQLite FTS5
   index (`docs.sqlite`), and an exact-name lookup table (`search_index.tsv`).
3. Agents query the derived corpus and verify load-bearing claims against the untouched
   originals.

Scope: the corpus contains what Unity's offline zip contains - the Manual and the Scripting
API reference. Some package manuals (URP, for example) are bundled into the Unity Manual;
most package API reference (`com.unity.*`) ships separately per package and is not included.

The full technical design - constraints, corpus format, benchmark methodology, and how
this differs from Context7, unity-api-mcp, and the docset ecosystem - is in
[docs/DESIGN.md](docs/DESIGN.md). The builder is Unity-specific; the pattern is not - a
separate write-up on the generic searchable-docs -> offline-fetch -> agent-transform ->
router-skill shape is planned.

## Quickstart

Requires Go 1.26+. Python 3 is optional (maintenance benchmarks only). The four steps below
are identical on Windows, macOS, and Linux - the tool is pure Go with no platform-specific
paths - and every command runs from the repository root.

Tested on: Windows (primary development platform, run end-to-end here) and Linux (the Go
build and full test suite run on `ubuntu-latest` in CI on every push - see the tests badge
above). macOS has no dedicated hardware in the loop; it shares the same pure-Go,
platform-neutral code path and is expected to work - reports welcome.

**1. Build the tools from source** (no prebuilt binaries are shipped):

```
git clone https://github.com/TotallyDomo/unity-doc-corpus
cd unity-doc-corpus
go build -C go -trimpath -o ../bin/ .
go build -C go -trimpath -o ../bin/ ./cmd/unity-doc-corpus-benchmark
```

Go names the binaries itself (`.exe` included on Windows) and writes them to `bin/`.
`scripts/build.ps1` (PowerShell) and `scripts/build.sh` (POSIX sh) are convenience wrappers
around exactly these two commands.

**2. Fetch Unity's official offline docs** (one-time per version; ~475 MB for 6000.3; the
zip's SHA-256 is printed and recorded in `unity-docs/.unity-doc-fetch`, and the zip itself
is kept in `unity-docs/` as the ground-truth artifact - pass `--delete-zip` to drop it):

```
bin/unity-doc-corpus fetch --version 6000.3
```

**3. Build the derived corpus** (writes `unity-docs/_agent`; ~45 s right after fetch,
longer on a cold file cache - the read stage is I/O-bound). After a successful build the
extracted HTML is pruned again - the retained zip can rematerialize it at any time, and a
later `build` does so automatically. Pass `--keep-source` to keep the extracted tree
around (you want this when iterating on the transform itself, and the `audit` verb needs
it on disk):

```
bin/unity-doc-corpus build --source unity-docs --output unity-docs/_agent
```

**4. Look something up** - built-in FTS5 search, no sqlite3 CLI or Python needed:

```
bin/unity-doc-corpus search "script execution order"
```

Steady-state footprint after these steps is ~665 MB of content: the ~475 MB zip plus
~190 MB of derived corpus (the corpus occupies more on disk - ~300 MB on a typical NTFS
volume - because 39k small files carry allocation overhead). Reading a page's original
HTML never needs a full re-extract -
`bin/unity-doc-corpus source Manual/execution-order.html` prints it straight from the zip.
If even the zip is too much, delete it (or fetch with `--delete-zip`) for a ~190 MB
footprint; everything keeps working except offline verification and offline rebuilds -
originals are then a pinned online fetch of each page's frontmatter `canonical_url`, and a
rebuild is a re-fetch.

## Agent skills

Two Claude Code skills live under `skills/`:

- **`unity-docs`** - lookup. Searches the built corpus for Unity Manual / Scripting API
  answers, with a verify-against-source step. This is the one you want day to day.
- **`unity-doc-corpus`** - builder/maintenance. Fetch, build, refresh, compare, benchmark.
  Expensive and explicitly triggered; never fires on ordinary doc questions.

Install both skills for Claude Code with:

```
npx skills add TotallyDomo/unity-doc-corpus --skill "*" --agent claude-code --copy -y
```

The explicit flags are deliberate: the CLI's interactive selector makes it easy to install
only one of the two skills, and on Windows its default symlink install can fail silently,
leaving the skills only under `.agents/skills/` - a directory Claude Code does not read.
`--copy` writes real files into `.claude/skills/` instead. Use `--skill unity-docs` for
lookup only, or `-g` to install user-globally rather than per-project. Prefer not to run
`npx` at all? The skills are plain Markdown - copy `skills/unity-docs` (and optionally
`skills/unity-doc-corpus`) into `.claude/skills/` yourself. The corpus itself is still
built once via the Quickstart above.

## Trust surface

- **Network**: `fetch` talks only to Unity's official documentation locations, all pinned
  in `go/fetch.go`: `docs.unity3d.com` to resolve the zip URL, and Unity's
  `docscloudstorage` bucket for the zip itself - via `cloudmedia-docs.unity3d.com` (current
  streams) or `storage.googleapis.com/docscloudstorage/` (2019.4 and older). The pinning
  holds on every hop: redirects off those hosts are refused, not followed. Nothing else
  fetches anything at runtime; the lookup skill is pure local reads, with one named
  exception - verifying a page against its original when neither the extracted HTML nor
  the retained zip is on disk means fetching that page's `canonical_url` from
  `docs.unity3d.com`, and the skill says so when it does.
- **Download integrity**: Unity publishes no checksum for the zip, so TLS to the pinned
  hosts is the integrity control on the download itself. `fetch` prints the zip's SHA-256
  and records it (with the version and URL) in a `.unity-doc-fetch` marker so you can pin
  a known-good value across re-fetches. The per-page SHA-256 chain in the corpus proves
  each derived page matches the HTML on your disk - consistency, not provenance.
- **Executes**: the two Go binaries you build from this repo's source, plus optional local
  Python scripts. No prebuilt binaries, no hooks. One exception to name plainly: the
  `npx skills add` install path below runs the third-party `skills` npm package; if that
  is outside your trust budget, copy the skill folders by hand instead - they are plain
  Markdown.
- **Data egress**: none.

## Legal posture

Unity's documentation belongs to Unity. This repository never contains or redistributes it -
not in the tree, not in git history. You download the official offline zip from Unity
yourself; the derived corpus stays on your machine.

## Support

Built for my own workflow. Will provide occasional updates if needed.

## License

MIT.
