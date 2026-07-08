#!/usr/bin/env python3
import argparse
import json
import re
import sqlite3
import time
from pathlib import Path


DEFAULT_CASES = [
    {
        "name": "Rigidbody.MovePosition API",
        "query": "Rigidbody.MovePosition moves rigidbody position",
        "expected": "ScriptReference/Rigidbody.MovePosition.html",
    },
    {
        "name": "DestroyImmediate API",
        "query": "Object.DestroyImmediate destroys object immediately edit mode",
        "expected": "ScriptReference/Object.DestroyImmediate.html",
    },
    {
        "name": "PrefabUtility instantiate",
        "query": "PrefabUtility.InstantiatePrefab instantiate prefab asset",
        "expected": "ScriptReference/PrefabUtility.InstantiatePrefab.html",
    },
    {
        "name": "BuildPipeline.BuildPlayer API",
        "query": "BuildPipeline.BuildPlayer build player options scenes locationPathName",
        "expected": "ScriptReference/BuildPipeline.BuildPlayer.html",
    },
    {
        "name": "Dynamic Resolution manual",
        "query": "dynamic resolution supported platforms render targets",
        "expected": "Manual/DynamicResolution-introduction.html",
    },
    {
        "name": "Script execution order manual",
        "query": "script execution order event functions update fixedupdate awake",
        "expected": "Manual/execution-order.html",
    },
    {
        "name": "YAML class ID reference",
        "query": "YAML class ID reference MonoBehaviour GameObject",
        "expected": "Manual/ClassIDReference.html",
    },
    {
        "name": "Coroutines manual",
        "query": "coroutines yield return WaitForSeconds",
        "expected": "Manual/Coroutines.html",
    },
]


def tokenize(query):
    return [t.lower() for t in re.findall(r"[A-Za-z0-9_.-]+", query) if len(t) > 1]


def fts_tokenize(query):
    return [t.lower() for t in re.findall(r"[A-Za-z0-9]+", query) if len(t) > 1]


def score_text(text, terms):
    lower = text.lower()
    missing = [term for term in terms if term not in lower]
    if missing:
        return 0
    return sum(lower.count(term) for term in terms)


def collect_source_files(source):
    files = []
    for section in ["Manual", "ScriptReference"]:
        root = source / section
        if root.exists():
            files.extend(sorted(root.rglob("*.html")))
    return files


def collect_derived_files(corpus):
    return sorted((corpus / "text").rglob("*.md"))


def rel_source(path, source):
    return path.relative_to(source).as_posix()


def source_rel_from_md(path, corpus):
    text = path.read_text(encoding="utf-8", errors="replace")
    match = re.search(r"^source_rel:\s*(.+)$", text, re.MULTILINE)
    return match.group(1).strip() if match else path.relative_to(corpus / "text").as_posix()


def load_pages_jsonl(corpus):
    records = []
    path = corpus / "pages.jsonl"
    if not path.exists():
        return records
    with path.open("r", encoding="utf-8") as handle:
        for line in handle:
            if line.strip():
                records.append(json.loads(line))
    return records


def coverage_report(source, corpus, records):
    source_rels = {p.relative_to(source).as_posix() for p in collect_source_files(source)}
    derived_rels = {r["source_rel"] for r in records}
    missing_records = sorted(source_rels - derived_rels)
    extra_records = sorted(derived_rels - source_rels)
    missing_markdown = sorted(r["md_rel"] for r in records if not (corpus / r["md_rel"]).exists())
    empty_content = [r["source_rel"] for r in records if r.get("content_chars", 0) == 0]
    return {
        "source_html_count": len(source_rels),
        "pages_jsonl_count": len(records),
        "missing_record_count": len(missing_records),
        "extra_record_count": len(extra_records),
        "missing_markdown_count": len(missing_markdown),
        "empty_content_count": len(empty_content),
        "missing_record_sample": missing_records[:20],
        "extra_record_sample": extra_records[:20],
        "missing_markdown_sample": missing_markdown[:20],
        "empty_content_sample": empty_content[:20],
    }


def sqlite_fts_search(corpus, query, limit=10):
    db_path = corpus / "docs.sqlite"
    if not db_path.exists():
        return None
    terms = fts_tokenize(query)
    if not terms:
        return None
    fts_query = " ".join(terms)
    start = time.perf_counter()
    conn = sqlite3.connect(db_path)
    try:
        rows = conn.execute(
            "SELECT p.source_rel, p.md_rel, bm25(pages_fts) AS rank FROM pages_fts JOIN pages p ON p.page_key = pages_fts.page_key WHERE pages_fts MATCH ? ORDER BY rank LIMIT ?",
            (fts_query, limit),
        ).fetchall()
    except sqlite3.OperationalError as exc:
        return {"error": str(exc), "query": fts_query}
    finally:
        conn.close()
    elapsed_ms = round((time.perf_counter() - start) * 1000, 3)
    return {
        "elapsed_ms": elapsed_ms,
        "query": fts_query,
        "hit_count": len(rows),
        "hits": [{"source_rel": row[0], "display_rel": row[1], "rank": row[2]} for row in rows],
    }


def search_files(files, terms, root, mode, limit=10):
    start = time.perf_counter()
    hits = []
    bytes_scanned = 0
    for path in files:
        raw = path.read_bytes()
        bytes_scanned += len(raw)
        text = raw.decode("utf-8", errors="replace")
        score = score_text(text, terms)
        if score:
            if mode == "source":
                source_rel = rel_source(path, root)
                display_rel = source_rel
            else:
                source_rel = source_rel_from_md(path, root)
                display_rel = path.relative_to(root).as_posix()
            hits.append({"score": score, "source_rel": source_rel, "display_rel": display_rel})
    elapsed_ms = round((time.perf_counter() - start) * 1000, 3)
    hits.sort(key=lambda h: (-h["score"], h["source_rel"]))
    return {
        "elapsed_ms": elapsed_ms,
        "bytes_scanned": bytes_scanned,
        "hit_count": len(hits),
        "hits": hits[:limit],
    }


def run_benchmark(source, corpus, cases):
    source_files = collect_source_files(source)
    derived_files = collect_derived_files(corpus)
    records = load_pages_jsonl(corpus)
    report = {
        "source": str(source),
        "corpus": str(corpus),
        "source_file_count": len(source_files),
        "derived_file_count": len(derived_files),
        "coverage": coverage_report(source, corpus, records),
        "cases": [],
    }
    source_total_bytes = sum(p.stat().st_size for p in source_files)
    derived_total_bytes = sum(p.stat().st_size for p in derived_files)
    report["source_html_bytes"] = source_total_bytes
    report["derived_markdown_bytes"] = derived_total_bytes
    report["derived_to_source_ratio"] = round(derived_total_bytes / source_total_bytes, 4) if source_total_bytes else None
    for case in cases:
        terms = tokenize(case["query"])
        source_result = search_files(source_files, terms, source, "source")
        derived_result = search_files(derived_files, terms, corpus, "derived")
        sqlite_result = sqlite_fts_search(corpus, case["query"])
        expected = case["expected"]
        source_recall = any(hit["source_rel"] == expected for hit in source_result["hits"])
        derived_recall = any(hit["source_rel"] == expected for hit in derived_result["hits"])
        sqlite_recall = (
            any(hit["source_rel"] == expected for hit in sqlite_result.get("hits", []))
            if isinstance(sqlite_result, dict) and "hits" in sqlite_result
            else False
        )
        exact_lookup = any(r["source_rel"] == expected and (corpus / r["md_rel"]).exists() for r in records)
        report["cases"].append(
            {
                "name": case["name"],
                "query": case["query"],
                "expected": expected,
                "terms": terms,
                "source": source_result,
                "derived": derived_result,
                "sqlite_fts": sqlite_result,
                "source_top10_recall": source_recall,
                "derived_top10_recall": derived_recall,
                "sqlite_top10_recall": sqlite_recall,
                "exact_lookup_available": exact_lookup,
                "derived_speedup_vs_source": round(source_result["elapsed_ms"] / derived_result["elapsed_ms"], 3)
                if derived_result["elapsed_ms"]
                else None,
                "sqlite_speedup_vs_source": round(source_result["elapsed_ms"] / sqlite_result["elapsed_ms"], 3)
                if isinstance(sqlite_result, dict) and sqlite_result.get("elapsed_ms")
                else None,
            }
        )
    report["source_top10_recall_count"] = sum(1 for c in report["cases"] if c["source_top10_recall"])
    report["derived_top10_recall_count"] = sum(1 for c in report["cases"] if c["derived_top10_recall"])
    report["sqlite_top10_recall_count"] = sum(1 for c in report["cases"] if c["sqlite_top10_recall"])
    report["exact_lookup_available_count"] = sum(1 for c in report["cases"] if c["exact_lookup_available"])
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description="Benchmark source Unity HTML docs against an agent corpus.")
    parser.add_argument("--source", required=True)
    parser.add_argument("--corpus", required=True)
    parser.add_argument("--cases", help="Optional JSON file with benchmark cases.")
    parser.add_argument("--output", help="Output JSON report path. Defaults to <corpus>/benchmark-report.json.")
    args = parser.parse_args(argv)
    cases = DEFAULT_CASES
    if args.cases:
        cases = json.loads(Path(args.cases).read_text(encoding="utf-8"))
    source = Path(args.source).resolve()
    corpus = Path(args.corpus).resolve()
    report = run_benchmark(source, corpus, cases)
    output = Path(args.output).resolve() if args.output else corpus / "benchmark-report.json"
    output.write_text(json.dumps(report, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print(json.dumps(report, indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
