---
name: unity-docs
description: Look up Unity engine documentation (Manual and Scripting API) offline from the locally built doc corpus - exact API/class pages, manual and concept pages, version-pinned engine behavior. Use for Unity documentation questions when a corpus has been built; do not use to build, refresh, or benchmark the corpus itself - that is the unity-doc-corpus skill.
---

# unity-docs (lookup)

Offline lookup over the derived Unity documentation corpus produced by the `unity-doc-corpus`
builder skill. All reads are local; nothing is fetched (the one exception is named in step 5
and the trust surface).

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

1. Once per session, read `manifest.json` and note its `unity_version`. If the question is
   about a different Unity version, say so - answer from the corpus but flag the mismatch.
   (`unity_version` is `unknown` when the docs were not fetched by this tool - then ask the
   user which version their docs are.)
2. Exact API, class, or page names: search `search_index.tsv`. It is a plain TSV with header
   `page_key  section  page_id  title  source_rel  md_rel  canonical_url` - grep the title or
   page_id column, e.g. `grep -i "AsyncOperation" search_index.tsv | head`.
3. Concept or free-text queries: FTS5 over `docs.sqlite`. If the `unity-doc-corpus` builder
   binary is on hand (you built the repo), it runs the query with no other tooling:

   ```
   unity-doc-corpus search --corpus <corpus-root> "script execution order"
   ```

   Otherwise query `docs.sqlite` directly with any local SQLite client - the `sqlite3` CLI,
   or Python's built-in `sqlite3` module if the CLI is missing:

   ```
   sqlite3 <corpus-root>/docs.sqlite "SELECT p.title, p.md_rel FROM pages_fts f JOIN pages p ON p.page_key = f.page_key WHERE pages_fts MATCH 'script execution order' ORDER BY bm25(pages_fts, 0.0, 10.0, 1.0) LIMIT 10;"
   ```

   The bm25 weights (title 10x body) matter: unweighted bm25 buries short canonical pages
   under their member pages. The built-in `search` verb applies them for you.
4. Open the matching stripped Markdown page under `text/Manual/...` or
   `text/ScriptReference/...` (the `md_rel` column points at it).
5. When an answer hinges on one page's exact details (table values, code, version
   caveats), re-read the original HTML - the transform is deliberately lossy (tables
   flatten, code loses fencing) and the corpus is a lookup cache, not the source of truth.
   Resolve the original from the page's frontmatter `source_rel`, in order:
   - the extracted tree: `<docs-root>/<source_rel>` (present when built with
     `--keep-source`);
   - the retained zip: `unity-doc-corpus source <source_rel>` prints the page from
     whichever of the two is on disk;
   - neither on disk: fetch the frontmatter `canonical_url` (docs.unity3d.com,
     version-pinned) - and note in your reply that you verified online, since this is the
     one step that leaves the machine. Compare content, not hashes: Unity republishes doc
     builds, so the online page may differ byte-wise from the fetched-zip original.
   This is insurance against transform bugs, not a routine second read - most lookups end
   at step 4.

## Answer contract

State the corpus Unity version and the page(s) used; separate what was read directly from a
page from what was inferred across pages.

## Trust surface

- Network: none for lookups - local file reads and local SQLite queries. One named
  exception: verifying a page when neither the extracted HTML nor the retained zip is on
  disk fetches that page's `canonical_url` from docs.unity3d.com, announced when it
  happens (see step 5).
- Executes: nothing required beyond your local grep/SQLite tooling; optionally the local
  `unity-doc-corpus search` binary if you use it.
- Data egress: none.
