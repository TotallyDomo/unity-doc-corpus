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
- **Offline-first and deterministic.** Normal lookup and source verification need no
  network while the retained zip or extracted HTML exists: no rate limits or API keys,
  and the same input produces the same derived page and index content. If both local
  original forms are deliberately removed, exact verification can fall back to one
  pinned Unity page online.
- **Verifiable.** A stripped, transformed page is a cache, not a source of truth. The
  agent must be able to walk back to the untouched original for load-bearing claims.

The naive offline baseline - grepping the official offline documentation zip - fails
the first requirement badly (648 MB of HTML for Unity 6000.3) and, as the benchmark
below shows, is ~50x slower per lookup and far worse at finding concept pages.

## Architecture at a glance

```
Unity offline zip
  -> fetch -> retained zip + disposable extracted HTML
  -> build -> stripped Markdown in SQLite + FTS5/TSV indexes + metadata
  -> lookup -> search -> `page` read -> original-source verification when needed
```

The retained zip is the local source artifact, the extracted HTML is a rebuild cache, and
the derived corpus is the cheap lookup representation. Exact-name lookup handles APIs,
FTS5 handles concepts, and the source path plus SHA-256 on every derived page closes the
loop back to the original bytes.

## Constraints

1. **No Unity content redistribution.** Unity's documentation belongs to Unity. The
   repository contains tooling only; the user fetches Unity's official offline zip
   themselves and the corpus is derived locally. Nothing doc-derived is ever committed -
   not in the tree, not in git history.
2. **Pure Go, CGO-free.** The builder and benchmark are single static binaries built
   from source (`modernc.org/sqlite`, no C toolchain). No prebuilt binaries are shipped;
   the build recipe is two `go build` lines.
3. **Plain-file outputs.** Consumers are agents with stock tools: grep, SQLite, and the
   local builder binary. No server process, no daemon, no protocol dependency. The corpus
   is a SQLite database plus small text metadata/index files.
4. **Originals untouched.** Both writers are marker-guarded: `build` only replaces an
   output directory carrying its own corpus marker, and `fetch --force` only replaces a
   destination carrying its own fetch marker. Neither deletes a directory it did not
   create. Source HTML, assets, and docdata stay intact next to the derived corpus.
5. **Rebuild is cheap.** A Unity version bump means re-fetch and re-build; the build
   must be fast enough that pinning to a new version is a non-event (~45 s wall clock on
   8 workers for the full 39k-page set right after fetch; the read stage is I/O-bound,
   so a cold file cache costs 2-3x that).

## Design

### Pipeline

1. **fetch** resolves the offline-docs zip URL from `docs.unity3d.com` and downloads it
   from Unity's `docscloudstorage` bucket - `cloudmedia-docs.unity3d.com` for current
   streams, `storage.googleapis.com/docscloudstorage/` for 2019.4 and older; all locations
   pinned in `go/fetch.go`, enforced on every redirect hop (an off-host redirect fails the
   download), and nothing else is ever fetched. It prints the zip's SHA-256, extracts just
   the `Manual/` and `ScriptReference/` subtrees (the only parts `build` reads) in a
   parallel worker pool, and stamps a `.unity-doc-fetch` marker (version, zip URL, zip
   SHA-256, retained zip name) that `build` surfaces as `unity_version` in
   `manifest.json`. Unity publishes no reference checksum for the zip: TLS to the pinned
   hosts is the integrity control, and the corpus's per-page SHA-256 chain proves
   derived-page-to-local-HTML consistency, not provenance.
2. **build** walks the `Manual/` and `ScriptReference/` HTML trees and transforms every
   page in a worker pool (default: half the logical CPUs). Output order is deterministic:
   files are discovered in sorted order and results are written by job index, so two
   builds of the same zip preserve page order and produce the same rendered page payloads
   and `search_index.tsv`.

### Artifact lifecycle

The retained zip is the ground-truth artifact; the extracted HTML tree is a disposable
cache materialized from it. `fetch` keeps the zip in the destination directory
(`--delete-zip` opts out), and `build` prunes the extracted tree after a successful build -
but only when both proofs are present: the `.unity-doc-fetch` marker (the tool created the
tree, it is not someone's own docs directory) and the retained zip it names (nothing is
lost). A later `build` rematerializes the tree from the zip automatically; `--keep-source`
keeps it around, which is what you want while iterating on the transform itself. The
`source` verb prints any page's original HTML from whichever home is on disk - the
extracted tree or, via cheap random access, the zip member - so per-page verification
never needs a full re-extract. A fresh Unity 6000.3 rebuild on 2026-07-13 measured the
retained zip at 452 MiB and the derived corpus at 123 MiB (five files, including a 112 MiB
`docs.sqlite`), for a 575 MiB steady state. Delete the zip too and verification falls back
to fetching the page's `canonical_url` online.

### HTML transform

The parser (`go/html_parser.go`) is a single-pass tag scanner written for Unity's
uniform doc-page structure, not a general HTML engine. Content extraction is anchored
at the `content-wrap` container; page chrome is dropped by a class/id/role blocklist
(sidebar, header, footer, toolbar, search, feedback, version switcher, language
switcher, navigation, breadcrumbs, prev/next page arrows, glossary-tooltip popups,
the Switch to Manual button).
`script`/`style`/`svg` subtrees are skipped outright. The parser
collects the title (overridden by Unity's own `docdata/index.json` title table when
present), headings, anchor links with their text, the canonical URL, and the flattened
body text with block-level tags mapped to newlines.

This is the lossy step, and it is deliberately biased toward recall of *text*: tables
flatten, code blocks lose fencing, inline markup disappears. The bet is that an agent
searching and skimming needs the words and the structure landmarks (headings, links),
and anything load-bearing gets verified against the original anyway (see below).
The result is a 90.5% byte reduction (648 MB -> 62 MB for Unity 6000.3).

Lossy in structure still needs a strong content-preservation guard. The `audit` verb
(`bin/unity-doc-corpus audit --source unity-docs --corpus unity-docs/_agent --baseline docs/audit-baseline-6000.3.json --shared-baseline docs/shared-content-baseline-6000.3.json`,
run after every transform change) re-extracts every page's visible text with an
independent extractor that shares no code with the production parser, shingles it, and
flags a page when a run of page-unique shingles is missing from its derived Markdown read
from `docs.sqlite`'s `page_text` table,
with a gating ratio-collapse tier plus an advisory ratio-outlier tier as gross-truncation
backstops and a source-vs-corpus page-count gate against silent whole-page loss. It
requires the extracted HTML tree (`build --keep-source`), covers the full corpus in
seconds, and gates on a checked-in baseline of individually triaged false positives
([audit-baseline-6000.3.json](audit-baseline-6000.3.json) - 496 pages, one known
footer-adjacency class), so only new flags fail a run. Baseline entries pin each accepted
page's flag magnitude and the corpus page count, so an accepted page that worsens - or a
corpus that quietly shrinks - re-gates instead of hiding behind its allowlist entry. The
baseline also pins every gate-affecting audit threshold and refuses a mismatched run. A
second `--shared-baseline` manifest extends the guard to corpus-common shared content
(see the false-negative note below). The flag is optional for ad hoc corpora, but the
checked-in Unity 6000.3 maintenance gate above requires its checked-in manifest. The
guard exists because the invariant silently failed once: a parser depth-tracking bug
(fixed 2026-07-09) truncated entire sections while the unit tests, the recall benchmark,
and the opt-in per-page verify step all stayed green.

#### What the audit proves - and what it does not

Precisely stated, a passing audit proves: **every page listed in the corpus has no run of
consecutive page-unique word shingles missing from its derived Markdown beyond the
accepted baseline, no gating ratio collapse, and the page count matches the source
tree with an exact one-to-one source-path match.** That is a strong new-regression detector,
not a mathematical lossless proof. The
known false-negative classes, stated with the same candor as the false-positive floor
above (established by an independent adversarial evaluation, 2026-07-12):

- **Corpus-common content (now detected, M0042-S6).** A shingle repeated on more than a
  handful of pages (shared boilerplate sentences like the `hideFlags` description, on 327
  pages) is by definition not page-unique, so the page-local check ignores it. This class
  is now caught by discriminating shared *content* from chrome via the shingle's
  derived-Markdown document frequency (mdDF): the two populations are sharply bimodal
  (measured on the clean corpus - 90.5% content at mdDF ~ refDF, 9.5% chrome at mdDF 0,
  0.05% ambiguous). A high-ref-DF shingle that is present across the Markdown broadly but
  missing from one page is a miss (live, no extra state; catches a partial strip). A
  *total* corpus-wide strip drops the shingle's mdDF to 0, which is indistinguishable from
  chrome without a recorded prior, so it is caught against an optional shared-content
  manifest (`--shared-baseline`
  [shared-content-baseline-6000.3.json](shared-content-baseline-6000.3.json)): a pinned
  shingle still shared in the source HTML but vanished from the Markdown gates the run.
- **Word-token granularity.** Both sides tokenize to letter/digit runs, so punctuation,
  operators, and signs are invisible to the invariant: `return -1` degrading to
  `return 1` does not flag. "Lossless" throughout this document means
  *word-token-lossless*; for exact code semantics, the per-page source verification path
  is the guarantee.
- **Stream edges.** A loss shorter than the run threshold hard against the very start or
  end of a page's visible-text stream can fall under the bar; interior losses down to a
  single word token clear it. On real pages, kept boundary chrome usually rescues edge
  detection.
- **Duplicate-page families.** Near-identical pages push all their shingles above the
  document-frequency cutoff, so the shingle invariant cannot see loss within the family;
  the gating ratio-collapse tier is the backstop there (it catches a blanked or gutted
  family member, not a small nibble).
- **Repeated content within one page.** Page-local shingle comparison is presence-based,
  not multiplicity-based. If the same paragraph appears twice and the transform drops one
  copy, the surviving copy can satisfy every shingle; the ratio-collapse tier only catches
  large losses of this shape.
- **Reordering.** The invariant checks presence, not order: paragraphs shuffled within a
  page do not flag. That is consistent with "lossy in structure" but worth stating.

### Corpus format

Per page, the builder renders Markdown with a frontmatter block carrying `section`,
`page_id`, `title`, `source_rel`, `source_sha256`, and `canonical_url`, followed by the
extracted content and a deduplicated link list. It stores those exact rendered bytes in the
`page_text` table in `docs.sqlite`, keyed by `page_key`; `unity-doc-corpus page <page_key>`
prints them. There is no `text/` directory. The frontmatter is the verification path:
`source_rel` points at the untouched original HTML and `source_sha256` proves which bytes
were transformed.

Corpus-level artifacts:

- `search_index.tsv` - one row per page (key, section, id, title, source path, canonical
  URL). The exact-name lane: whatever text-search tool is on hand (`rg` or `grep`, e.g.
  `rg -i "AsyncOperation" search_index.tsv`) answers API-name lookups without touching a
  database.
- `docs.sqlite` - the `pages` metadata table, the `page_text` read table holding the exact
  rendered Markdown, and a contentless `pages_fts` FTS5 table. The FTS table stores only its
  inverted index, so the body exists once in `page_text`; it is queried with bm25 at a 10:1
  title:body weighting (unweighted bm25 buries short canonical pages under their member
  pages; the weight is measured, see the benchmark). If the SQLite driver lacks FTS5 the
  build degrades gracefully and records the fact in the manifest.
- `manifest.json` - build summary: `unity_version` (from the fetch marker; `unknown` when
  the docs were not fetched by this tool), page count, byte totals, derived/source ratio,
  per-stage timings, worker count.
- `index.md` + a `.unity-doc-agent-corpus` marker (the overwrite guard).

### Lookup path (the skills)

Two portable Agent Skills under `skills/`, packaged for Claude Code and Codex, split the
cost model. `unity-docs` (lookup) is
the cheap, day-to-day path - exact names via the TSV, concepts via FTS5 (the built-in
`unity-doc-corpus search` verb, or any SQLite client), then `unity-doc-corpus page <page_key>`
to read the Markdown from SQLite, then verify against the original for load-bearing claims. The
`unity-doc-corpus` skill (builder/maintenance) owns fetch/build/benchmark and only
fires on explicit request, so an ordinary doc question never triggers a several-hundred-MB
fetch. Lookup and verification are fully offline while the retained zip or extracted HTML
exists; if both were removed, the lookup skill may fetch one pinned Unity page for announced
source verification. Each skill also carries optional `agents/openai.yaml` presentation
metadata for Codex, which Claude Code ignores.

## Benchmark

Claims about "better lookup" are cheap; the benchmark binary
(`go/cmd/unity-doc-corpus-benchmark`) makes them measurable and reproducible:

```
bin/unity-doc-corpus-benchmark --corpus unity-docs/_agent
bin/unity-doc-corpus-benchmark --corpus unity-docs/_agent --extended
bin/unity-doc-corpus-benchmark --source unity-docs --corpus unity-docs/_agent --comparison
```

**Modes and cases.** The default routine check runs only the shipped corpus FTS5 lane: 8
curated lookups plus a frozen 1,000-case title-derived sample. `--extended` keeps the fast
FTS-only lane but uses 10,000 evenly sampled four-term snippets from page bodies, excluding
title and page-id terms. This harder distribution exercises concept retrieval without
scaling the expensive scan lanes. `--comparison` runs the frozen title-derived cases
through four strategies: (a) naive term-scoring over raw HTML, (b) the same scan over
derived Markdown, (c) FTS5 bm25 over raw HTML, and (d) FTS5 bm25 over the shipped corpus.
Every tier is deterministic - no RNG, same corpus in, same cases out. A case is recalled
when its expected page appears in the top 10 results.

**Comparison results** (Unity 6000.3, 39,056 pages, 1008 cases, 8 workers; the checked-in
reference run is [benchmark-6000.3.json](benchmark-6000.3.json) - consecutive runs
reproduce every recall count exactly):

| Lane | Top-10 recall (all) | Manual pages only | Mean query time |
| --- | --- | --- | --- |
| Raw HTML, naive scan | 945/1008 (93.8%) | 55/93 (59.1%) | ~207 ms |
| Derived Markdown, naive scan | 958/1008 (95.0%) | 58/93 (62.4%) | ~42 ms |
| FTS5 bm25 over raw HTML | 977/1008 (96.9%) | 89/93 (95.7%) | ~3.5 ms |
| FTS5 bm25 over the corpus (shipped) | 976/1008 (96.8%) | 89/93 (95.7%) | ~4.2 ms |

A fresh 2026-07-13 rebuild after the storage consolidation reproduced the two FTS5 recall
counts exactly: 977/1008 over raw HTML and 976/1008 over the shipped corpus. Moving the
page-read payload into `docs.sqlite` therefore changed the storage layout, not ranking.

The four lanes form a ranker x representation matrix, and reading it honestly:

- **Ranking, not the transform, owns recall.** Holding bm25 fixed and swapping the
  representation (raw HTML vs derived corpus) moves recall by one case in a thousand.
  Anyone with the offline zip and SQLite can have this recall without this tool - the raw
  index just costs ~860 MB on disk versus the corpus's ~84 MB, and every page read out of
  it costs ~9x the bytes in an agent's context.
- **Where ranking matters is concept pages.** On ScriptReference cases every lane
  saturates - an API page's own name is a near-unique string, so even a naive scan finds
  it. Manual concept pages, whose titles are ordinary prose, are where naive scanning
  collapses (59-62%) and bm25 holds (~95%).
- **The title weighting is measured, not aesthetic.** Unweighted bm25 buries short
  canonical pages under their member pages (the class page for a bare class-name query);
  the 10:1 title:body weighting is worth +0.6 points overall and turned the benchmark's
  longest-standing curated miss (`script execution order`) into a #1 hit. The
  `search_index.tsv` exact-name lane still answers bare API names without the database.
- **Speed separates FTS from scanning, not the FTS lanes from each other** (~4 ms either
  way once an index exists; a naive scan pays ~207 ms per query against raw HTML).

**Honest limits.** The frozen default and comparison cases use page titles as queries -
self-retrieval by a page's own name - which favors every lane and makes API-name cases
outright easy. The extended tier replaces titles with body snippets, but it is still
self-retrieval from target-page text rather than a real information need. The separate
fixed 100-query [concept suite](concept-queries-6000.3.json) measures hand-curated
agent-style requests; retrieval-lever outcomes are recorded in
[retrieval-evaluations-6000.3.md](retrieval-evaluations-6000.3.md). Recall@10 says nothing
about precision or answer quality. The shipped
FTS5 lane is only one lane of the actual lookup path: the skills route exact API names
through `search_index.tsv` first, which covers bm25's characteristic miss (bare class
names ranked below their member pages). The naive-scan baseline scans page content only
and does not model filename matching, which a real grep workflow would use for API
names; it also holds the whole corpus in memory, which flatters its times. Timings are
from one machine (8-worker desktop). The benchmark measures retrieval, not end-to-end
token savings in a live agent session. Erratum: this repository's initially published
numbers (91.9% FTS5 vs 57.1% "grep") overstated the recall story twice over - the
generated cases were the head of the sorted page list (100% Manual pages in a 91%
ScriptReference corpus), and the decisive baseline, bm25 over the raw HTML, was missing
entirely. Both are corrected above; the ranking also gained the measured title weighting
and the transform now strips tooltip/switch-link chrome it previously kept. Full history
in git.

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
no rate limits or API key, is deterministic (same zip -> same derived page/index content,
with a SHA-256 chain from every derived page back to its source bytes), and ships a
published recall benchmark.
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
  4-11x token savings in its workflows and self-reports 100% search relevance on sampled
  queries; it ships no reproducible benchmark artifact behind either number.

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
questions. The benchmark artifact is the sharpest edge - none of the tools above ship a
reproducible retrieval benchmark (unity-api-mcp self-reports relevance numbers, but with
no artifact to rerun).
