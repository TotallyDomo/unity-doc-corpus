# Bounded transform residency

Measured 2026-10-27 for M438-S3, after failure-safe publication (M438-S1)
and linear normalization (M438-S2). Baseline source: commit
`3067c0d`; candidate changes only ordered transform scheduling, not parsing,
normalization, rendering, SQLite schema, or publication.

## Matched full-corpus comparison

Unity 6000.3, 39,056 pages. Retained source zip SHA-256:
`0d776d70548fb00d0c9f6c763e6fc3517fa73e6f8aea3bf235bc43fcf94a75b9`.
Go 1.26.6 windows/amd64, modernc.org/sqlite 1.61.0, eight workers,
GOMAXPROCS=8, GOGC=100, GOMEMLIMIT=off, source retained.

Both variants use the same test-only measurement wrapper around the real builder.
The baseline overlays the pre-change build.go; the wrapper does not change the
builder. Runtime MemStats deltas cover allocations and collections. Windows
GetProcessMemoryInfo reports the process peak working set, not a sampled heap
estimate. Build wall time excludes process startup and source pre-reading.

Every run pre-reads the same sorted source tree. A baseline/candidate warm-up pair
is excluded; measured order is baseline, candidate, candidate, baseline. This is a
warm-cache, shared-desktop measurement, not a cold-cache or isolated-load claim.

| Metric | Baseline runs | Bounded runs |
| --- | --- | --- |
| Peak working set, MiB | 1509.3, 1528.8 | 90.7, 85.2 |
| Build wall time, seconds | 12.256, 14.630 | 18.519, 20.456 |
| Allocated bytes, billions | 12.8847, 12.8847 | 12.8798, 12.8798 |
| Allocations, millions | 26.6004, 26.6003 | 26.6101, 26.6111 |
| GC cycles | 68, 67 | 849, 834 |

Median peak working set falls 94.2%; median wall time increases 45.0% (about six
seconds). Total allocation is effectively unchanged. The justification is releasing
more than 1.4 GiB of retained memory with a small bounded pipeline, not a speedup.
More frequent collection accompanies the lower live heap; no global GC tuning or
cross-build cache is introduced. Results do not establish performance on other
machines or worker counts.

## Correctness and integration

- The window admits at most twice the worker count of unconsumed pages, including
  transforms in progress. A slow head page cannot grow a corpus-sized reorder buffer.
- Unit checks cover delayed first results, deterministic order, bounded admission,
  empty/small inputs, worker/consumer errors, cancellation, and worker joining.
  A scratch mutation replacing the bound with the complete job count fails all
  three bounded-order cases (one, two, and eight workers).
- The complete four-package Windows Go suite and go vet pass. Existing real-I/O
  failure tests retain the previous readable corpus and source, without staging
  leftovers, for read, SQLite, TSV, close/commit and metadata failures.
- An isolated rebuild through the actual CLI passes. All 39,056 rendered pages,
  metadata/source hashes, TSV bytes, SQLite schema and every table ordered by rowid
  (including FTS internals) match the baseline; both databases pass integrity_check.
- Baseline and candidate content audits match after excluding elapsed time, and
  the fixed title-derived retrieval reports match after excluding timing/path.
  No audit allowlists or retrieval cases were refreshed.

The full-corpus check integrates the final S1 publication and S2 normalization
paths; original HTML and the pinned zip remain available. No Unity content is in
this report. Raw run logs, commands, source hashes, overlays and measurement wrappers
remain in the local task evidence directory, not in published artifacts.
