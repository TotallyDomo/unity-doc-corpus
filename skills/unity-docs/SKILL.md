---
name: unity-docs
description: Look up Unity engine documentation (Manual and Scripting API) offline from the locally built doc corpus - exact API/class pages, manual and concept pages, version-pinned engine behavior. Use for Unity documentation questions when a corpus has been built; do not use to build, refresh, or benchmark the corpus itself - that is the unity-doc-corpus skill.
---

# unity-docs (lookup)

Offline lookup over the derived Unity documentation corpus produced by the `unity-doc-corpus`
builder skill. All reads are local; nothing is fetched.

The corpus root is the builder's `--output` directory - by default `unity-docs/_agent` under
the repository where the builder ran. A valid corpus root contains a `.unity-doc-agent-corpus`
marker file plus `manifest.json`, `search_index.tsv`, `docs.sqlite`, and `text/`. If no corpus
root is known or present, ask the user to run the builder quickstart first (a one-time
several-hundred-MB fetch plus a fast local build); do not build it unprompted.

Scope: the corpus holds Unity's Manual and Scripting API reference (what the official offline
zip contains). Some package manuals (URP, for example) are bundled into the Manual, but most
package API reference (`com.unity.*`) is not included - if a package-API lookup comes up
empty, say the corpus does not cover it rather than concluding the API does not exist.

## Lookup order

1. Once per session, read `manifest.json` and note the corpus Unity version. If the question
   is about a different Unity version, say so - answer from the corpus but flag the mismatch.
2. Exact API, class, or page names: search `search_index.tsv`. It is a plain TSV with header
   `page_key  section  page_id  title  source_rel  md_rel  canonical_url` - grep the title or
   page_id column, e.g. `grep -i "AsyncOperation" search_index.tsv | head`.
3. Concept or free-text queries: FTS5 over `docs.sqlite`. If the `unity-doc-corpus` builder
   binary is on hand (you built the repo), it runs the query with no other tooling:

   ```
   unity-doc-corpus search --corpus <corpus-root> "addressables memory"
   ```

   Otherwise query `docs.sqlite` directly with any local SQLite client - the `sqlite3` CLI,
   or Python's built-in `sqlite3` module if the CLI is missing:

   ```
   sqlite3 <corpus-root>/docs.sqlite "SELECT p.title, p.md_rel FROM pages_fts f JOIN pages p ON p.page_key = f.page_key WHERE pages_fts MATCH 'addressables memory' ORDER BY bm25(pages_fts) LIMIT 10;"
   ```
4. Open the matching stripped Markdown page under `text/Manual/...` or
   `text/ScriptReference/...` (the `md_rel` column points at it).
5. For load-bearing conclusions, verify against the original HTML: each Markdown page's
   frontmatter records its source path and SHA-256. The corpus is a lookup cache, not the
   source of truth.

## Answer contract

State the corpus Unity version and the page(s) used; separate what was read directly from a
page from what was inferred across pages.

## Trust surface

- Network: none. All lookups are local file reads and local SQLite queries.
- Executes: nothing required beyond your local grep/SQLite tooling; optionally the local
  `unity-doc-corpus search` binary if you use it.
- Data egress: none.
