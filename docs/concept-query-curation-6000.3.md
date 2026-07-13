# Concept query curation - Unity 6000.3

The concept evaluator has two intentionally separate inputs:

- `concept-queries-6000.3.json` is the fixed 100-query development set.
- `concept-queries-6000.3-heldout.json` is the fixed 200-query held-out set.

The development cases may be used to explore candidate retrieval strategies. The held-out
cases are a decision gate: do not inspect their search outcomes, add aliases for their
misses, or change query-relaxation parameters based on them until a candidate is frozen.
Run both only after the candidate's behavior is fixed.

## Curation method

Each case starts as a compact agent-style information need - a task, symptom, or desired
behavior rather than a copied document title, source path, or API identifier. The matching
gold `source_rel` is then checked in the untouched local Unity 6000.3 source tree. At run
time the evaluator checks every gold page against the `pages` table before it searches, so
a stale or mistyped page fails the measurement instead of silently becoming a miss.

The held-out suite balances Manual and Scripting API cases at 100 each. It includes APIs
because agents frequently ask for a behavior without knowing the identifier, but API names
are deliberately absent from the queries. It includes broad Manual coverage across animation,
physics, audio, rendering, lighting, terrain, UI Toolkit, and packages.

Before check-in, review each held-out case for source-title leakage. The automated test rejects
duplicate queries, query overlap with the development set, and a copied consecutive pair of
meaningful source-path terms. This is a conservative proxy for title-copy leakage; manual
review remains necessary for semantic paraphrases that happen to reuse a title's vocabulary.

## Reproduce

Build the evaluator, then run each partition against the same corpus:

```powershell
go build -C go -trimpath -o ../bin/ ./cmd/unity-doc-corpus-concept-eval
bin/unity-doc-corpus-concept-eval --corpus unity-docs/_agent --policy exact
bin/unity-doc-corpus-concept-eval --corpus unity-docs/_agent --eval docs/concept-queries-6000.3-heldout.json --policy exact
```

Both text and JSON reports include overall recall plus separate Manual and Scripting API
recall. Use `--json` for a saved per-query hit/miss record. Omit `--policy exact` to measure
the adopted safe-fill policy instead.

## Frozen baseline

The untuned `unicode61` corpus recalled 3/200 held-out cases at top 10 (1.5%): 2/100
Manual cases and 1/100 Scripting API cases. This number is a baseline, not a target for
rewriting the held-out queries. Future candidates may compare against it only after their
design and parameters are fixed.
