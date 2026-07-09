# Design: a derived Unity documentation corpus for agents

Technical design doc for the corpus builder in this repository. It covers the problem,
the constraints that shaped the design, the corpus format, the benchmark methodology and
evidence, and how this differs from existing tools. It is versioned with the code; when
the builder changes, this document changes in the same tree.

Scope note: this is the technical layer. The narrative around the generic pattern
(searchable docs -> offline fetch -> agent-optimized transform -> cheap router skill)
belongs to a separate write-up and is deliberately not duplicated here.

## Problem

Coding agents answer Unity questions inside a context window billed per token, and they
answer them badly from training data alone: Unity ships several supported versions at
once, APIs deprecate across them, and hallucinated signatures look exactly like real
ones. The agent needs a documentation lookup path that is:

- **Cheap per lookup.** Every page an agent reads is paid for in tokens. Unity's doc
  pages carry navigation sidebars, headers, footers, feedback widgets, and version
  switchers around the actual content.
- **Version-pinned.** A project is on one Unity version. `docs.unity3d.com` URLs
  redirect across versions, and hosted indexes track whatever was scraped last.
- **Offline and deterministic.** No network dependency, no rate limits, no API keys,
  and the same input produces the same corpus.
- **Verifiable.** A stripped, transformed page is a cache, not a source of truth. The
  agent must be able to walk back to the untouched original for load-bearing claims.

The naive offline baseline - grepping the official offline documentation zip - fails
the first requirement badly (648 MB of HTML for Unity 6000.3) and, as the benchmark
below shows, is also much worse at actually finding the right page.

## Constraints

1. **No Unity content redistribution.** Unity's documentation belongs to Unity. The
   repository contains tooling only; the user fetches Unity's official offline zip
   themselves and the corpus is derived locally. Nothing doc-derived is ever committed -
   not in the tree, not in git history.
2. **Pure Go, CGO-free.** The builder and benchmark are single static binaries built
   from source (`modernc.org/sqlite`, no C toolchain). No prebuilt binaries are shipped;
   the build recipe is two `go build` lines.
3. **Plain-file outputs.** Consumers are agents with stock tools: grep, SQLite, a file
   reader. No server process, no daemon, no protocol dependency. Everything in the
   corpus is a text file or a SQLite database.
4. **Originals untouched.** The builder writes only into its output directory (guarded
   by a marker file so it never deletes a directory it did not create). Source HTML,
   assets, and docdata stay intact next to the derived corpus.
5. **Rebuild is cheap.** A Unity version bump means re-fetch and re-build; the build
   must be fast enough that pinning to a new version is a non-event (under a minute
   wall clock on 8 workers for the full 39k-page set).

## Design

### Pipeline

1. **fetch** resolves the offline-docs zip URL from `docs.unity3d.com` and downloads it
   from Unity's `docscloudstorage` bucket - `cloudmedia-docs.unity3d.com` for current
   streams, `storage.googleapis.com/docscloudstorage/` for 2019.4 and older; all locations
   pinned in `go/fetch.go` and nothing else is ever fetched - then extracts just the
   `Manual/` and `ScriptReference/` subtrees (the only parts `build` reads) in a parallel
   worker pool, straight to the destination.
2. **build** walks the `Manual/` and `ScriptReference/` HTML trees and transforms every
   page in a worker pool (default: half the logical CPUs). Output order is deterministic:
   files are discovered in sorted order and results are written by job index, so two
   builds of the same zip produce byte-identical `pages.jsonl` and `search_index.tsv`.

### HTML transform

The parser (`go/html_parser.go`) is a single-pass tag scanner written for Unity's
uniform doc-page structure, not a general HTML engine. Content extraction is anchored
at the `content-wrap` container; page chrome is dropped by a class/id/role blocklist
(sidebar, header, footer, toolbar, search, feedback, version switcher, language
switcher, navigation). `script`/`style`/`svg` subtrees are skipped outright. The parser
collects the title (overridden by Unity's own `docdata/index.json` title table when
present), headings, anchor links with their text, the canonical URL, and the flattened
body text with block-level tags mapped to newlines.

This is the lossy step, and it is deliberately biased toward recall of *text*: tables
flatten, code blocks lose fencing, inline markup disappears. The bet is that an agent
searching and skimming needs the words and the structure landmarks (headings, links),
and anything load-bearing gets verified against the original anyway (see below).
The result is an 88.3% byte reduction (648 MB -> 76 MB for Unity 6000.3).

### Corpus format

Per page, the builder emits a Markdown file under `text/<section>/<page_id>.md` with a
frontmatter block carrying `section`, `page_id`, `title`, `source_rel`,
`source_sha256`, and `canonical_url`, followed by the extracted content and a
deduplicated link list. The frontmatter is the verification path: `source_rel` points
at the untouched original HTML and `source_sha256` proves which bytes were transformed.

Corpus-level artifacts:

- `search_index.tsv` - one row per page (key, section, id, title, source path, md path,
  canonical URL). The exact-name lane: `grep -i "AsyncOperation" search_index.tsv`
  answers API-name lookups without touching a database.
- `docs.sqlite` - a `pages` metadata table plus a `pages_fts` FTS5 table (title + body)
  queried with bm25 ranking. If the SQLite driver lacks FTS5 the build degrades
  gracefully and records the fact in the manifest.
- `pages.jsonl` - the full per-page metadata records (byte counts, heading lists, link
  counts, both SHA-256 hashes) for tooling and benchmark-case generation.
- `manifest.json` - build summary: page count, byte totals, derived/source ratio,
  per-stage timings, worker count.
- `index.md` + a `.unity-doc-agent-corpus` marker (the overwrite guard).

### Lookup path (the skills)

Two Claude Code skills under `skills/` split the cost model: `unity-docs` (lookup) is
the cheap, day-to-day path - exact names via the TSV, concepts via FTS5 (the built-in
`unity-doc-corpus search` verb, or any SQLite client), then open the Markdown page, then
verify against the original for load-bearing claims. The
`unity-doc-corpus` skill (builder/maintenance) owns fetch/build/benchmark and only
fires on explicit request, so an ordinary doc question never triggers a several-hundred-MB
fetch. The lookup skill performs no network access at all.

## Benchmark

Claims about "better lookup" are cheap; the benchmark binary
(`go/cmd/unity-doc-corpus-benchmark`) makes them measurable and reproducible:

```
bin/unity-doc-corpus-benchmark --source unity-docs --corpus unity-docs/_agent --generated-cases 1000
```

**Cases.** 8 curated lookups (real agent-style queries: API signatures, manual concepts)
plus 1000 generated cases: for each of the first 1000 pages in `pages.jsonl`, the page
title (falling back to page id) becomes the query and that page is the expected answer.

**Lanes.** Each case runs against three search strategies: (a) naive term-scoring scan
over the raw offline HTML, (b) the same scan over the derived Markdown, and (c) SQLite
FTS5 with bm25 ranking. A case counts as recalled when the expected page appears in the
top 10 results.

**Results** (Unity 6000.3, 39,056 pages, 1008 cases, 8 workers - the checked-in
reference run is reproducible with the command above):

| Lane | Top-10 recall | Mean query time |
| --- | --- | --- |
| Raw HTML, naive scan | 576/1008 (57.1%) | ~292 ms |
| Derived Markdown, naive scan | 609/1008 (60.4%) | ~58 ms |
| SQLite FTS5 (bm25) | 926/1008 (91.9%) | ~7 ms |

Two separate effects are visible. Stripping chrome alone buys only a modest recall gain
(57% -> 60%) but a 5x scan speedup - the corpus is 11.7% of the source bytes. The recall
jump comes from ranking: bm25 over clean title+body text finds the right page in the
top 10 for 91.9% of cases, at ~42x the speed of scanning the raw HTML.

**Honest limits.** Generated cases use page titles as queries, which favors any
title-aware lane - the curated cases are closer to real agent queries but there are only
8 of them. Recall@10 says nothing about precision or answer quality. The residual ~8%
FTS5 misses are mostly pages whose titles share all their terms with many sibling pages
(landing pages, disambiguation-style manual pages). Timings are from one machine
(8-worker desktop) and the naive-scan lanes hold the whole corpus in memory, which
flatters their times. The benchmark measures retrieval, not end-to-end token savings in
a live agent session.

## Differentiation

Prior-art map re-verified 2026-07-09. The space splits into four categories; the
one-line disambiguation first: the various "Unity MCP" servers (Unity's official MCP
server, CoplayDev/unity-mcp, IvanMurzak/Unity-MCP, mcp-unity) are *editor-control
bridges* - they let an agent drive the Unity Editor. Different problem; none of them is
a documentation lookup system.

### Context7 (hosted docs index)

[Context7](https://github.com/upstash/context7) is the dominant "pull current docs into
the context window" tool: a hosted documentation database behind an MCP server.

**Finding (verified 2026-07-09): Context7 does index Unity documentation.** Its catalog
includes several Unity website scrapes - `/websites/unity` (~4.2M tokens, ~54k
snippets), `/websites/unity_en-us`, `/websites/unity3d_manual` - plus roughly thirty
Unity-adjacent libraries (Entities, Physics, Timeline, and other package docs). If you
are online and version-agnostic, Context7 already serves Unity doc snippets.

What it does not offer is exactly this project's niche: the corpus here is derived from
the specific offline zip *you* fetched for *your* Unity version, works air-gapped, has
no rate limits or API key, is deterministic (same zip -> same corpus, SHA-256 chain from
every derived page back to its source bytes), and ships a published recall benchmark.
A hosted service cannot be version-pinned to your project or audited page-by-page
against the originals on your disk.

### unity-api-mcp (closest neighbor)

[unity-api-mcp](https://github.com/Codeturion/unity-api-mcp) (verified 2026-07-09:
v2.0.2, Feb 2026, PolyForm Noncommercial) targets the same hallucinated-Unity-API
problem via MCP. Key differences:

- **Source and scope.** It builds from XML IntelliSense files and package doc-comments -
  Scripting API surface only, no Manual pages. This corpus transforms the official docs
  HTML and covers Manual + ScriptReference, so concept pages (execution order, physics
  settings, build pipeline behavior) are searchable alongside API reference.
- **Distribution posture.** It redistributes pre-built per-version SQLite databases
  (~18-24 MB) - Unity doc content in someone else's release artifacts. Here, nothing
  doc-derived leaves your machine and nothing doc-derived is in the repo; the legal
  posture is itself a design feature.
- **Runtime shape.** MCP server process vs. plain files any tool can grep. It claims
  4-11x token savings in its workflows; it publishes no retrieval-recall numbers.

### Docset ecosystem (Dash / Zeal / DevDocs)

[Dash](https://kapeli.com/dash) (which now ships a native MCP integration), Zeal,
[DocsetMCP](https://github.com/codybrom/DocsetMCP), and the DevDocs scrapers are the
mature "hundreds of offline doc corpora with per-source recipes" wheel, and Dash's
user-contributed catalog does include a Unity 3D docset. Docsets are optimized for
human browsing - original HTML plus a search index - not for token-lean agent reads;
coverage and freshness of user-contributed docsets vary; and none of the ecosystem
publishes retrieval benchmarks. Anyone generalizing this builder to many doc sources
should target the docset format rather than reinvent it - that is exactly the wheel it
already solved - but for the agent-lookup problem the docset answer is still "open the
original page."

### The gap this fills

As of 2026-07-09 no found tool ships the combination: official-offline-zip in,
locally-derived version-pinned corpus out (Manual + ScriptReference), a SHA-256
verification path from every derived page to its source, a published recall/latency
benchmark, and a two-skill router that keeps the expensive path from firing on ordinary
questions. The benchmark artifact is the sharpest edge - none of the tools above
publish recall numbers for their retrieval at all.
