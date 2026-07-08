---
name: unity-doc-corpus
description: Fetch Unity's official offline documentation and build, refresh, compare, or benchmark the derived agent corpus. Use only when explicitly asked to run or maintain the Unity doc corpus workflow (fetch, build, refresh, benchmark, compare); do not use for ordinary Unity documentation lookups or API questions - that is the unity-docs skill.
---

# unity-doc-corpus (builder)

Build or refresh the agent-friendly derived corpus from Unity's offline HTML documentation:
stripped Markdown, JSONL metadata, SQLite FTS5 index, exact-name lookup TSV, and benchmark
reports. This is the expensive, explicitly-triggered maintenance workflow; day-to-day doc
lookups belong to the `unity-docs` skill.

Run all commands from the repository root. Invocations are written suffix-less
(`bin/unity-doc-corpus`); on Windows they resolve to the `.exe` automatically.

## Workflow

1. Build the tools once, or when `go/` sources change (no prebuilt binaries ship in the repo):

   ```
   cd go
   go build -trimpath -o ../bin/unity-doc-corpus .
   go build -trimpath -o ../bin/unity-doc-corpus-benchmark ./cmd/benchmark
   cd ..
   ```

   Append `.exe` to the `-o` names on Windows, or use the wrapper:
   `powershell -ExecutionPolicy Bypass -File scripts/build.ps1`

2. Fetch the offline docs for the requested Unity version stream (~300 MB download from
   Unity's official hosts):

   ```
   bin/unity-doc-corpus fetch --version 6000.3
   ```

   `--destination` defaults to `unity-docs`; `--resolve-only` prints the resolved zip URL
   without downloading; `--force` re-fetches; the download cache defaults to
   `<os-temp>/unity-doc-downloads`.

3. Build or refresh the derived corpus:

   ```
   bin/unity-doc-corpus build --source unity-docs --output unity-docs/_agent
   ```

   `--workers` defaults to half the logical CPUs; lower it to keep the machine responsive.

4. Sanity-check the output: `unity-docs/_agent/manifest.json` reports page count and byte
   ratios; `unity-docs/_agent/index.md` describes the corpus layout and lookup order.

The builder never modifies the original HTML: it writes a derived `_agent` directory beside
it and records a SHA-256 per source page. Keep the originals intact - the lookup skill's
verification step depends on them.

## Maintenance (builder changes)

- Compare two corpora after changing the builder:
  `python scripts/compare_corpus.py --source unity-docs --baseline .cache/corpus-baseline --candidate unity-docs/_agent`
- Recall/efficiency benchmark against the original HTML:
  `python scripts/benchmark_corpus.py --source unity-docs --corpus unity-docs/_agent`
- Expanded Go benchmark with generated title/page-id cases:
  `bin/unity-doc-corpus-benchmark --source unity-docs --corpus unity-docs/_agent --generated-cases 1000 --output unity-docs/_agent/benchmark-report-expanded.json`
- Go toolchain changes: run `go mod tidy`, `go vet ./...`, and `go test ./...` from `go/`.

## Output contract

- `_agent/text/Manual/...md` and `_agent/text/ScriptReference/...md` - stripped page content.
- `_agent/pages.jsonl` - one record per page: source path, title, hashes, canonical URL,
  derived Markdown path, byte counts.
- `_agent/search_index.tsv` - exact-name lookup table (page_key, section, page_id, title,
  source_rel, md_rel, canonical_url).
- `_agent/docs.sqlite` - `pages` metadata plus `pages_fts` FTS5 table.
- `_agent/manifest.json` - generation summary; `_agent/benchmark-report*.json` - benchmarks.

## Trust surface

- Network: `fetch` downloads only from Unity's official documentation hosts
  (`docs.unity3d.com` resolves the zip URL; `cloudmedia-docs.unity3d.com` serves the zip -
  both pinned in `go/fetch.go`). Nothing else is fetched at runtime.
- Executes: the two Go binaries you build from this repository's source, plus the optional
  local Python maintenance scripts.
- Data egress: none. Everything runs and stays on the local machine.
