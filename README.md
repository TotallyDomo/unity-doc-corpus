# unity-doc-corpus

[![tests](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/tests.yml/badge.svg)](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/tests.yml)
[![govulncheck](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/govulncheck.yml/badge.svg)](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/govulncheck.yml)
[![gitleaks](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/gitleaks.yml/badge.svg)](https://github.com/TotallyDomo/unity-doc-corpus/actions/workflows/gitleaks.yml)

Turn Unity's official offline documentation into a version-pinned local search corpus for
coding agents: stripped Markdown served from SQLite for cheap reads, SQLite FTS5 for concept
search, and an exact-name index for API lookup. The pure-Go (CGO-free) builder runs entirely
on your machine.

No Unity documentation content lives in this repository. You fetch Unity's official
offline docs zip yourself, and the builder derives the corpus locally. The repository
ships tooling and portable Agent Skills for Claude Code and Codex.

## Fit and scope

Use this when you want Unity documentation that is:

- pinned to the Unity version your project actually uses;
- available offline without an API key, hosted index, or background service;
- compact enough for an agent to search and read without spending context on page chrome;
- traceable from every derived page back to the untouched source HTML.

The corpus contains the Manual and Scripting API reference from Unity's offline zip. Some
package manuals, including URP, are bundled into the Manual; most package API reference
(`com.unity.*`) ships separately and is not included. This is a local corpus builder, not
an editor-control MCP server and not a redistribution of Unity's documentation.

The default steady-state footprint for Unity 6000.3 is 575 MiB (603 MB), measured from a
fresh 2026-07-13 rebuild: a retained 452 MiB source zip plus a 123 MiB derived corpus. The
derived corpus is five files, not ~39k small Markdown files; `docs.sqlite` is 112 MiB and
holds both the page-read payload and the FTS index. You can delete the zip for a 123 MiB
footprint, but then exact source verification needs the pinned online page and rebuilding
needs a re-fetch.

## What it builds

| Artifact | Purpose |
| --- | --- |
| `docs.sqlite` | Per-page Markdown (`page_text`), metadata, and title-weighted FTS5 |
| `search_index.tsv` | Exact-name lookup for APIs and pages |
| `manifest.json` | Unity version, page counts, sizes, and build summary |

Every Markdown page records the original source path and SHA-256. The transform is
deliberately lossy in structure - tables flatten and code loses fencing - so agents can
verify load-bearing details against the retained original with one local command.

## Quickstart

Requires Go 1.26+. Python 3 is optional and used only by maintenance benchmarks. Run all
commands from the repository root.

Tested end to end on Windows. The Go build and full test suite run on Linux in CI on every
push. macOS uses the same pure-Go, platform-neutral path but has no dedicated hardware in
the test loop.

**1. Build the tools from source** (no prebuilt binaries are shipped):

```
git clone https://github.com/TotallyDomo/unity-doc-corpus
cd unity-doc-corpus
go build -C go -trimpath -o ../bin/ .
go build -C go -trimpath -o ../bin/ ./cmd/unity-doc-corpus-benchmark
go build -C go -trimpath -o ../bin/ ./cmd/unity-doc-corpus-concept-eval
```

Go names the binaries itself (`.exe` included on Windows) and writes them to `bin/`.
`scripts/build.ps1` and `scripts/build.sh` wrap the same three commands.

**2. Fetch Unity's official offline docs** (one time per version):

```
bin/unity-doc-corpus fetch --version 6000.3
```

The zip's SHA-256 is printed and recorded in `unity-docs/.unity-doc-fetch`. The zip stays
in `unity-docs/` as the local ground-truth artifact; pass `--delete-zip` to drop it.

**3. Build the derived corpus:**

```
bin/unity-doc-corpus build --source unity-docs --output unity-docs/_agent
```

The reference build takes about 45 seconds immediately after fetch on an 8-worker desktop;
budget 2-3x on a cold file cache. After a successful build, extracted HTML is pruned when
the retained zip can rematerialize it. Pass `--keep-source` while developing the transform
or before running the audit.

**4. Search and read:**

```
bin/unity-doc-corpus search "script execution order"
bin/unity-doc-corpus page Manual/execution-order
```

`search` returns page keys; `page` prints the matching stripped Markdown from
`docs.sqlite`. No sqlite3 CLI or Python is needed. To inspect the untouched page behind a
result:

```
bin/unity-doc-corpus source Manual/execution-order.html
```

## Agent integration

The corpus and CLI are agent-agnostic. The two standard Agent Skills under `skills/` are
packaged for Claude Code and Codex:

- **`unity-docs`** - day-to-day lookup. Searches the corpus and verifies load-bearing
  details against the original.
- **`unity-doc-corpus`** - fetch, build, refresh, compare, audit, and benchmark. It only
  triggers for an explicit corpus-maintenance request.

From the project where the agent will use them, install both skills for Claude Code and
Codex with:

```
npx skills add TotallyDomo/unity-doc-corpus --skill "*" --agent claude-code --agent codex --copy -y
```

This creates project-local copies in `.claude/skills/` for Claude Code and
`.agents/skills/` for Codex. `--copy` avoids Windows symlink problems. For a user-global
installation, add `-g` and verify the result with `npx skills list -g --agent codex` because
agent-linking behavior has varied across CLI releases. Select one `--agent` and/or one
`--skill` when you only want part of the integration. The skill folders are also
self-contained Markdown packages: you can copy them manually to the agent's project or
global skill directory instead of running `npx`. Codex-specific display metadata lives in
each skill's `agents/openai.yaml`; Claude Code ignores it.

If the skills are installed outside this repository, tell the agent where the built corpus
lives when it is not discoverable at `unity-docs/_agent` from the current project.

## Measured results

Reference corpus: Unity 6000.3. The checked-in run is
[`docs/benchmark-6000.3.json`](docs/benchmark-6000.3.json).

| Metric | Value |
| --- | --- |
| Pages transformed | 39,056 |
| Source HTML | 648 MB |
| Derived Markdown payload | 62 MB (9.5% of source bytes, stored in `docs.sqlite`) |
| Corpus FTS5 top-10 recall | 96.8% (976/1008) |
| Same bm25 over raw HTML | 96.9% (977/1008) |
| Corpus `docs.sqlite` vs. raw FTS5 index | 112 MiB vs. ~860 MB |
| Mean corpus FTS5 query | ~4.2 ms |

The important result is recall parity, not a claim that stripping HTML improves ranking:
with the same ranker, the corpus and raw HTML differ by one case in 1000. A fresh
post-consolidation rebuild reproduced both FTS5 counts exactly. The gain is that the pages
agents read are about 90% smaller and the equivalent search index is about 7x smaller.
Generated cases use page titles and ids, so they are easier than real agent questions; the
benchmark's limits and four-lane methodology are documented in
[`docs/DESIGN.md`](docs/DESIGN.md#benchmark).

The separate `audit` command guards the transform against word-token content loss. It
independently re-extracts all source pages, gates on new or worsening missing-content flags,
checks exact source-to-corpus page coverage, and carries a manifest for shared content that
page-local checks cannot detect. This is a strong regression detector, not a mathematical
proof: punctuation-only changes, short stream-edge losses, duplicate-page families,
repeated content, and reordering have documented blind spots. Exact code or table semantics
remain the job of per-page source verification. See
[`docs/DESIGN.md`](docs/DESIGN.md#what-the-audit-proves---and-what-it-does-not) for the
precise contract and the checked-in baselines.

Run the fixed 1,000-case title-derived regression benchmark with:

```
bin/unity-doc-corpus-benchmark --corpus unity-docs/_agent
```

Use `--extended` for the fixed 10,000-case body-snippet recall tier. Use
`--comparison --source unity-docs` for the slower four-lane FTS-versus-scan comparison.

Measure the separate, hand-curated concept-query suite with per-query hits and misses:

```
bin/unity-doc-corpus-concept-eval --corpus unity-docs/_agent
```

The fixed 100-case development suite in [`docs/concept-queries-6000.3.json`](docs/concept-queries-6000.3.json)
and separate 200-case held-out suite in [`docs/concept-queries-6000.3-heldout.json`](docs/concept-queries-6000.3-heldout.json)
measure agent-style requests with verified gold source pages. Both are balanced between Manual
and Scripting API coverage, and the evaluator reports each section separately. Curation and
the held-out decision gate are documented in [`docs/concept-query-curation-6000.3.md`](docs/concept-query-curation-6000.3.md).
This is intentionally distinct from the benchmark's title-derived sample.

## Architecture

```
Unity offline zip
  -> fetch -> retained zip + disposable extracted HTML
  -> build -> page Markdown in SQLite + FTS5/TSV indexes + metadata
  -> lookup -> search -> `page` read -> original-source verification when needed
```

`fetch` only accepts Unity's pinned documentation hosts and extracts the Manual and
ScriptReference trees. `build` transforms each page and stores the byte-identical rendered
Markdown in `docs.sqlite`; it writes the TSV exact-name index alongside it. Agents use
exact-name lookup for API pages, FTS5 for concepts, `page` for the read payload, and the
untouched HTML for details the lossy representation cannot preserve exactly.

The full technical design covers constraints, artifact lifecycle, corpus format, audit
semantics, benchmark methodology, and comparisons with Context7, unity-api-mcp, and the
docset ecosystem: [`docs/DESIGN.md`](docs/DESIGN.md).

## Trust surface

- **Network**: `fetch` talks only to Unity documentation hosts pinned in `go/fetch.go`:
  `docs.unity3d.com`, `cloudmedia-docs.unity3d.com`, and the legacy
  `storage.googleapis.com/docscloudstorage/` bucket. Redirects off the allowlist fail.
  Lookup is fully offline while the extracted source or retained zip exists. If neither
  exists, the lookup skill may fetch one page's pinned `canonical_url` from
  `docs.unity3d.com` for announced source verification.
- **Download integrity**: Unity publishes no checksum for the zip, so TLS to pinned hosts
  is the download integrity control. `fetch` records the resolved URL and SHA-256; per-page
  hashes prove consistency with the local HTML, not provenance from Unity.
- **Executes**: the three Go binaries built from this repository, plus optional local Python
  maintenance scripts. The documented `npx skills add` path executes the third-party
  [`skills`](https://github.com/vercel-labs/skills) package; copy the plain skill folders
  manually if that is outside your trust budget.
- **Data egress**: none.
- **Pinned audit point**: the annotated `v1.0.0` tag is the fixed release to audit and pin
  (`git clone --branch v1.0.0 https://github.com/TotallyDomo/unity-doc-corpus`); `main`
  moves, the tag does not.
- **Content is data**: corpus pages are Unity's documentation text. Agents consuming search
  results and page reads should treat that text as material to read, never as instructions
  to follow.

## Legal posture

Unity's documentation belongs to Unity. This repository never contains or redistributes it
- not in the tree and not in git history. You download the official zip, and the derived
corpus stays on your machine.

## Support

Built for my own workflow. Occasional updates as needed.

## License

MIT.
