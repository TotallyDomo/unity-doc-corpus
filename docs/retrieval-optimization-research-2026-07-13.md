# Retrieval optimization research - 2026-07-13

Planning input for M52. The production corpus remains on SQLite FTS5's default
`unicode61` tokenizer. This note preserves the reasoning, local experiments, and
recommended execution order behind the post-Porter retrieval plan.

## Problem and constraints

- The corpus is local, offline, pure Go, and served from SQLite FTS5.
- Search quality is measured at top-10 against three distributions: 100 curated
  agent-style concept queries, the frozen 1,008-case title-derived suite, and the
  10,008-case body-derived extended suite.
- The exact-name TSV lane already handles canonical API names. FTS changes should
  target concept and natural-language lookup rather than duplicate substring lookup.
- The current evaluator and benchmark sanitize a query into terms and join them with
  spaces. FTS5 treats that as implicit `AND`, so one vocabulary mismatch can shrink or
  empty the result set.
- Any adopted lever must preserve the content audit and avoid regressions on the frozen
  and extended retrieval suites.

## Measured baselines and Porter result

| Strategy | Concept | Frozen title-derived | Extended body-derived |
| --- | ---: | ---: | ---: |
| Default `unicode61` | 58/100 | 976/1008 | 9264/10008 |
| Global `porter unicode61` | 64/100 | 974/1008 | 9098/10008 |

Porter improved the curated concept suite by six cases, but lost two frozen cases and
166 extended cases. The broader regression showed that globally conflating inflections
changes corpus statistics and competition between pages, not just query coverage. The
candidate passed the content audit but was rejected under the non-regression gate. Full
details are in [retrieval-evaluations-6000.3.md](retrieval-evaluations-6000.3.md).

## Query-relaxation prototype

A read-only one-off Python experiment queried the existing `docs.sqlite`; it made no
repository or corpus changes. It recreated the same title and body case generation used
by the Go benchmark and tested two retrieval policies:

1. Safe fill: keep the exact implicit-AND result order. If fewer than 10 rows are
   returned, run all leave-one-term-out variants plus a full-OR variant, rank unseen
   candidates with reciprocal-rank fusion (RRF), and append only into unused slots.
2. Aggressive RRF: fuse the exact, leave-one-term-out, and full-OR ranked lists into a
   new top 10.

| Strategy | Concept | Frozen title-derived | Extended body-derived |
| --- | ---: | ---: | ---: |
| Exact implicit `AND` | 58/100 | 976/1008 | 9264/10008 |
| Safe relaxed fill | 63/100 | 976/1008 | 9264/10008 |
| Exact/relaxed aggressive RRF | 66/100 | 976/1008 | Not run at full 10k |

Safe fill gained `manual-028`, `api-020`, `api-021`, `api-039`, and `api-048` without
displacing any baseline result. Aggressive RRF added three more concept hits and lost
none on the 100 concept cases or the frozen title suite. This is exploratory evidence,
not an adoption result: the same 100 queries were used to discover the lever, and the
aggressive policy still needs the full held-out and extended gates.

The concept baseline has 42 misses. Seventeen return fewer than 10 hits, including two
empty result sets. Sparse result lists are also common in the synthetic suites: 791/1008
frozen queries and 7231/10008 extended queries return fewer than 10 rows. A naive policy
that launches every relaxation variant whenever the list is short would therefore
multiply query work for most requests. The production prototype should first test a
small trigger, one document-frequency-guided relaxation, and a zero-result OR fallback.

## Recommended execution order

### 1. Expand and hold out the concept eval

Add at least 200 independently verified agent-style queries, preserving the original
100 as a development set and treating the new set as held out until each lever's
parameters are fixed. Keep Manual and Scripting API reporting separate. The synthetic
10k suite remains a regression guard, not a substitute for real information needs.

### 2. Adaptive query relaxation and fusion

Prototype this before changing the index:

- Run the current exact implicit-AND query first.
- Use `fts5vocab` document counts to identify the least discriminative query term.
- When the trigger fires, run one query with that term removed. Use full OR only for an
  empty result or as a final fallback.
- Compare safe append, exact-biased RRF, and fully fused RRF. RRF is appropriate because
  BM25 scores from different query expressions are not directly comparable.
- Preserve exact query results and sanitized-query behavior.
- Measure p50/p95 latency and query count in addition to recall. Reject a quality gain
  whose routine latency multiplier is disproportionate.

The safe-append variant is the lowest-risk starting point because it cannot displace an
existing top-10 hit. The aggressive variant has higher measured upside but requires the
full regression gate.

### 3. Heading-weighted FTS column

Continue M52-S3 after the held-out eval and relaxation decision. Add a heading column
between title and body and test a small predeclared weight grid, for example heading
weights 2, 3, and 5 with title=10/body=1. Do not derive evaluation queries from headings.

### 4. Document-aware passage indexing

Continue M52-S4 after the heading result. Index per-heading passages for long Manual
pages, but carry the page title and owning heading into every passage. Collapse passage
hits back to one page result using max score or rank fusion. Passage-only rows without
document context risk solving length dilution while losing the information that names
the page's subject.

### 5. Unity-aware query aliases and identifier decomposition

Prefer query-side expansion or an isolated weighted field over global token replacement.
Start with a small reviewed map and identifier rules, such as:

- `asmdef` <-> `assembly definition`
- `fps` <-> `frame rate`
- `gameobject` <-> `game object`
- CamelCase decomposition such as `DontDestroyOnLoad` -> `dont destroy on load`
- Narrow Unity-relevant morphology such as `async` <-> `asynchronous`

Keep aliases inspectable and provenance-backed. Avoid a general WordNet-style expansion
that introduces unrelated senses.

### 6. Conditional Porter sidecar

Only if residual held-out misses are genuinely morphological after the earlier levers,
test a separate Porter candidate index while retaining `unicode61` as the authoritative
ranker. Query the sidecar only on sparse or low-confidence results and combine candidates
with exact-biased fusion. Measure index size and latency. This is preferable to another
global tokenizer swap but less attractive than schema-free relaxation.

## Deferred options

Do not add these to M52 until the lexical plan reaches a measured ceiling:

- Generated question/document expansion for Manual pages in a low-weight field. This
  directly targets vocabulary mismatch but adds generation provenance, determinism, and
  evaluation-leakage concerns.
- Learned sparse retrieval such as SPLADE, or dense embeddings fused with FTS. These may
  improve semantic coverage but add model/runtime dependencies and conflict with the
  current compact pure-Go/offline trust surface.
- Trigram or prefix indexes as a concept-retrieval lever. They improve substring or
  prefix matching and query speed, while exact API names are already served separately.
- Custom BM25 constants as the first response to long pages. SQLite's built-in BM25 uses
  fixed length-normalization constants; passage indexing attacks that problem more
  directly.

## Primary references

- [SQLite FTS5 documentation](https://www.sqlite.org/fts5.html): implicit AND, Boolean
  queries, NEAR, per-column BM25 weights, tokenizer synonyms, and `fts5vocab`.
- [Reciprocal Rank Fusion outperforms Condorcet and individual rank learning methods](https://cormack.uwaterloo.ca/cormacksigir09-rrf.pdf).
- [DAPR: A Benchmark on Document-Aware Passage Retrieval](https://aclanthology.org/2024.acl-long.236/).
- [Document Expansion by Query Prediction](https://arxiv.org/abs/1904.08375).
- [SPLADE v2: Sparse Lexical and Expansion Model for Information Retrieval](https://arxiv.org/abs/2109.10086).
