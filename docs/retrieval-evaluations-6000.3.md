# Retrieval evaluations - Unity 6000.3

This log records measured retrieval-lever decisions against the fixed 100-query
agent-style concept suite and the frozen 1,008-case title-derived recall benchmark.
Candidate corpora are built from the same local Unity 6000.3 source tree as the
reference corpus. A lever is adopted only when it improves concept recall without
regressing the title-derived benchmark or the content audit.

## Porter stemming - rejected (2026-07-13)

Hypothesis: changing `pages_fts` from SQLite FTS5's default `unicode61` tokenizer to
`porter unicode61` would improve morphological matching for concept queries. The
candidate was built in an isolated corpus with `tokenize='porter unicode61'`; the
FTS5 `MATCH` path tokenizes both indexed text and incoming query terms, so no separate
query-side stemming code was required.

| Measurement | Default `unicode61` | Candidate `porter unicode61` | Delta |
| --- | ---: | ---: | ---: |
| Concept query recall@10 | 58/100 (58.0%) | 64/100 (64.0%) | +6 |
| Frozen title-derived recall@10 | 976/1008 (96.8%) | 974/1008 (96.6%) | -2 |
| Extended body-derived recall@10 | 9264/10008 (92.6%) | 9098/10008 (90.9%) | -166 |
| Manual title-derived recall@10 | 89/93 | 89/93 | 0 |
| ScriptReference title-derived recall@10 | 887/915 | 885/915 | -2 |

The candidate passed the content audit: 39,056 pages, zero new content-loss flags,
and zero collapsed shared-content shingles. It was not adopted because it regressed the
frozen title-derived suite by two cases and the broader body-derived suite by 166 cases.
The production schema continues to use the default `unicode61` tokenizer.

Follow-up research, measured query-relaxation prototypes, and the recommended execution
order are recorded in
[retrieval-optimization-research-2026-07-13.md](retrieval-optimization-research-2026-07-13.md).

## Held-out concept baseline - frozen (2026-07-13)

The 200-query held-out suite was measured on the unchanged production `unicode61` corpus
after curation and before further retrieval tuning. It returned 3/200 gold pages at top 10
(1.5%): 2/100 Manual cases and 1/100 Scripting API cases. This is a deliberately separate
decision gate, not a source for selecting a retrieval lever or rewriting queries. The suite,
curation rules, and reproducible commands are in
[concept-query-curation-6000.3.md](concept-query-curation-6000.3.md).

## Adaptive query relaxation - adopted as safe fill (2026-07-13)

The evaluated policy leaves FTS content unchanged and adds only the metadata-only
`pages_fts_vocab` virtual table. It normalizes a natural-language query into quoted literal terms, runs
the strict implicit-AND query first, and only relaxes a result set with fewer than seven
hits. `fts5vocab` document counts select the highest-frequency (least discriminative)
term to remove. A full OR query is permitted only as a final fallback after an empty
strict result and an insufficient relaxed result.

`safe-fill` appends unseen relaxed or fallback hits after the exact ranking, so every
exact result stays in the same position. `fused` uses deterministic reciprocal-rank
fusion with a 4:1 exact-to-relaxed weight and retains the exact prefix, but supplied no
additional recall on any suite. The simpler safe-fill policy is now the default lookup
path. `exact` remains available on both evaluation commands as the baseline policy.

| Policy | Development concept | Held-out concept | Frozen title-derived | Extended body-derived |
| --- | ---: | ---: | ---: | ---: |
| Exact | 58/100 | 3/200 | 976/1008 | 9264/10008 |
| Safe fill - adopted | 59/100 | 44/200 | 976/1008 | 9264/10008 |
| Exact-biased fused | 59/100 | 44/200 | 976/1008 | 9264/10008 |

The adopted policy improves the frozen held-out set by 41 cases without regressing either
synthetic suite. The development gain is one case because the seven-hit trigger deliberately
leaves broad but merely short result sets on the one-query path.

All timings include opening the local SQLite database, term-frequency lookup, and retrieval
on the rebuilt Unity 6000.3 corpus. Exact uses one FTS query per case. Safe-fill and fused
have the same query counts; their p50/p95 timings vary slightly between runs.

| Suite | Safe fill p50/p95 | Safe fill FTS queries/case | Fused p50/p95 | Fused FTS queries/case |
| --- | ---: | ---: | ---: | ---: |
| Development (100) | 1.753 / 5.998 ms | 1.31 | 1.562 / 3.598 ms | 1.31 |
| Held out (200) | 19.882 / 82.778 ms | 2.54 | 18.915 / 80.923 ms | 2.54 |
| Frozen title-derived (1008) | 3.087 / 10.011 ms | 1.66 | 3.027 / 10.004 ms | 1.66 |
| Extended body-derived (10008) | 5.920 / 11.975 ms | 1.66 | 5.933 / 12.000 ms | 1.66 |

The held-out distribution is unusually sparse, which explains its higher fallback rate and
tail latency. The production trigger keeps the much larger synthetic distributions below two
FTS queries per case while preserving their exact recall.

## Heading-weighted FTS column - rejected (2026-07-13)

Hypothesis: headings extracted by the HTML parser would provide a high-signal FTS field when
weighted between the existing title (10) and body (1) fields. The candidate rebuilt the same
39,056-page Unity 6000.3 corpus with parser headings in a separate contentless FTS5 column.
The predeclared weight grid was 2, 3, and 5; all three returned the same development concept
recall, so the upper-grid candidate (5) was carried through the independent gates.

| Measurement | Baseline | Heading weight 5 | Delta |
| --- | ---: | ---: | ---: |
| Development concept recall@10 | 59/100 | 59/100 | 0 |
| Held-out concept recall@10 | 44/200 | 46/200 | +2 |
| Frozen title-derived recall@10 | 976/1008 | 978/1008 | +2 |
| Extended body-derived recall@10 | 9264/10008 | 9242/10008 | -22 |

The candidate passed the content audit: 39,056 pages, 496 baselined flags, zero new or stale
flags, and zero collapsed shared-content shingles. It was rejected because the extended
body-derived regression violates the non-regression gate, despite the small held-out and frozen
gains. No heading column or weighting change was kept.

This reconciles M51-S4's metadata cleanup: headings remain transient parser output and are not
preserved in docs.sqlite because this retrieval lever was not adopted.

## Per-heading passage indexing - rejected (2026-07-13)

Hypothesis: replacing one whole-page FTS row with per-heading rows for long Manual pages would
reduce BM25 length dilution. The measured prototype split only Manual pages with at least 4,000
body bytes and two parsed passages. Each row carried the page title plus its owning heading and
body, and ranked passage hits were collapsed to the owning page by best passage rank. Short
Manual pages and all Scripting API pages retained one row.

The prototype split 829 pages and expanded the FTS index from 39,056 to 46,188 rows. The largest
fan-out was `Manual/Glossary.html` at 473 passages. `docs.sqlite` grew from 117,301,248 to
118,120,448 bytes (+819,200 bytes, 0.7%).

| Measurement | Page-level baseline | Passage candidate | Delta |
| --- | ---: | ---: | ---: |
| Development concept recall@10, safe fill | 59/100 | 58/100 | -1 |
| Development Manual recall@10, safe fill | 28/50 | 27/50 | -1 |
| Held-out concept recall@10, safe fill | 44/200 | 55/200 | +11 |
| Held-out Manual recall@10, safe fill | 31/100 | 39/100 | +8 |
| Frozen title-derived recall@10 | 976/1008 | 980/1008 | +4 |
| Extended body-derived recall@10 | 9264/10008 | 9260/10008 | -4 |
| Extended Manual recall@10 | 916 | 913 | -3 |

The strict exact-policy result did not support the passage-ranking hypothesis: development
recall fell from 58/100 to 56/100 (Manual 28/50 to 26/50), while held-out recall stayed 3/200.
The safe-fill held-out gain came from changed passage-level document frequencies and relaxed
ranking, not from better strict passage retrieval. That side effect also produced three held-out
Scripting API gains even though Scripting API pages were not split. It is not a stable basis for
replacing the authoritative page ranker.

The candidate passed the content audit unchanged: 39,056 pages, 496 baselined flags, zero new or
stale flags, and zero collapsed shared-content shingles. The prototype left `pages` and
`page_text` untouched, added a passage-row-to-page-row mapping for FTS, and collapsed read-path
search results back to page keys; `page` reads and audit semantics therefore remained per-page.

The candidate was rejected and its prototype code was removed because both the development
concept suite and extended regression suite declined. Production remains one FTS row per page.
If passage retrieval is revisited, use an isolated sidecar plus an explicitly evaluated fusion
policy instead of perturbing page-level FTS statistics, cap or exclude extreme glossary/changelog
fan-out, and use a newly held-out query set because this suite has now informed the design.
