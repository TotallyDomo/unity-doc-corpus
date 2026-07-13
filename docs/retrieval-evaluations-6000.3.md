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
