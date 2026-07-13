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
