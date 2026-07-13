#!/usr/bin/env python3
"""Compare two SQLite-backed Unity documentation corpora for logical equivalence."""

import argparse
import hashlib
import json
import sqlite3
from pathlib import Path


METADATA_FIELDS = (
    "section",
    "page_id",
    "title",
    "source_rel",
    "canonical_url",
    "source_sha256",
    "text_sha256",
    "source_bytes",
    "text_bytes",
)


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def blob_hex(value):
    if value is None:
        return None
    return bytes(value).hex()


def collect_source_rels(source):
    if not source:
        return set()
    rels = set()
    for section in ("Manual", "ScriptReference"):
        root = source / section
        if root.exists():
            rels.update(path.relative_to(source).as_posix() for path in root.rglob("*.html"))
    return rels


def load_corpus(corpus):
    db_path = corpus / "docs.sqlite"
    if not db_path.is_file():
        raise ValueError(f"missing docs.sqlite: {db_path}")

    conn = sqlite3.connect(db_path)
    try:
        rows = conn.execute(
            """
            SELECT p.page_key, p.section, p.page_id, p.title, p.source_rel,
                   p.canonical_url, p.source_sha256, p.text_sha256,
                   p.source_bytes, p.text_bytes, pt.md
            FROM pages p
            LEFT JOIN page_text pt ON pt.page_key = p.page_key
            """
        ).fetchall()
        try:
            fts_count = conn.execute("SELECT COUNT(*) FROM pages_fts").fetchone()[0]
            fts5 = True
        except sqlite3.OperationalError:
            fts_count = None
            fts5 = False
    finally:
        conn.close()

    pages = {}
    missing_page_text = []
    for row in rows:
        (
            page_key,
            section,
            page_id,
            title,
            source_rel,
            canonical_url,
            source_sha256,
            text_sha256,
            source_bytes,
            text_bytes,
            markdown,
        ) = row
        if markdown is None:
            missing_page_text.append(page_key)
        pages[page_key] = {
            "section": section,
            "page_id": page_id,
            "title": title,
            "source_rel": source_rel,
            "canonical_url": canonical_url,
            "source_sha256": blob_hex(source_sha256),
            "text_sha256": blob_hex(text_sha256),
            "source_bytes": source_bytes,
            "text_bytes": text_bytes,
            "markdown": markdown,
        }

    return {
        "pages": pages,
        "missing_page_text": sorted(missing_page_text),
        "fts5": fts5,
        "fts_count": fts_count,
    }


def compare(source, baseline, candidate, sample_limit):
    source_rels = collect_source_rels(source)
    baseline_corpus = load_corpus(baseline)
    candidate_corpus = load_corpus(candidate)
    baseline_pages = baseline_corpus["pages"]
    candidate_pages = candidate_corpus["pages"]

    baseline_keys = set(baseline_pages)
    candidate_keys = set(candidate_pages)
    missing_keys = sorted(baseline_keys - candidate_keys)
    extra_keys = sorted(candidate_keys - baseline_keys)
    field_mismatches = []
    markdown_mismatches = []
    source_hash_mismatches = []
    fts_mismatches = []

    for key in sorted(baseline_keys & candidate_keys):
        base = baseline_pages[key]
        cand = candidate_pages[key]
        for field in METADATA_FIELDS:
            if base[field] != cand[field]:
                field_mismatches.append(
                    {
                        "page_key": key,
                        "field": field,
                        "baseline": base[field],
                        "candidate": cand[field],
                    }
                )
                break
        if base["markdown"] != cand["markdown"]:
            markdown_mismatches.append({"page_key": key})
        if source:
            source_path = source / base["source_rel"]
            if source_path.is_file():
                actual_hash = sha256_bytes(source_path.read_bytes())
                if actual_hash != cand["source_sha256"]:
                    source_hash_mismatches.append(
                        {
                            "page_key": key,
                            "source_rel": base["source_rel"],
                            "expected": actual_hash,
                            "candidate": cand["source_sha256"],
                        }
                    )

    candidate_source_rels = {record["source_rel"] for record in candidate_pages.values()}
    missing_source_records = sorted(source_rels - candidate_source_rels) if source else []
    extra_source_records = sorted(candidate_source_rels - source_rels) if source else []
    for label, corpus_data, page_count in (
        ("baseline", baseline_corpus, len(baseline_pages)),
        ("candidate", candidate_corpus, len(candidate_pages)),
    ):
        if corpus_data["fts5"] and corpus_data["fts_count"] != page_count:
            fts_mismatches.append(
                {
                    "corpus": label,
                    "expected_page_count": page_count,
                    "actual_fts_count": corpus_data["fts_count"],
                }
            )
    if baseline_corpus["fts5"] != candidate_corpus["fts5"]:
        fts_mismatches.append(
            {
                "corpus": "pair",
                "baseline_fts5": baseline_corpus["fts5"],
                "candidate_fts5": candidate_corpus["fts5"],
            }
        )
    elif baseline_corpus["fts5"] and baseline_corpus["fts_count"] != candidate_corpus["fts_count"]:
        fts_mismatches.append(
            {
                "corpus": "pair",
                "baseline_fts_count": baseline_corpus["fts_count"],
                "candidate_fts_count": candidate_corpus["fts_count"],
            }
        )
    report = {
        "source": str(source) if source else None,
        "baseline": str(baseline),
        "candidate": str(candidate),
        "baseline_page_count": len(baseline_pages),
        "candidate_page_count": len(candidate_pages),
        "source_html_count": len(source_rels) if source else None,
        "missing_key_count": len(missing_keys),
        "extra_key_count": len(extra_keys),
        "field_mismatch_count": len(field_mismatches),
        "markdown_mismatch_count": len(markdown_mismatches),
        "source_hash_mismatch_count": len(source_hash_mismatches),
        "fts_mismatch_count": len(fts_mismatches),
        "missing_source_record_count": len(missing_source_records),
        "extra_source_record_count": len(extra_source_records),
        "sqlite": {
            "baseline": {
                "fts5": baseline_corpus["fts5"],
                "pages_fts": baseline_corpus["fts_count"],
                "missing_page_text_count": len(baseline_corpus["missing_page_text"]),
            },
            "candidate": {
                "fts5": candidate_corpus["fts5"],
                "pages_fts": candidate_corpus["fts_count"],
                "missing_page_text_count": len(candidate_corpus["missing_page_text"]),
            },
        },
        "samples": {
            "missing_keys": missing_keys[:sample_limit],
            "extra_keys": extra_keys[:sample_limit],
            "field_mismatches": field_mismatches[:sample_limit],
            "markdown_mismatches": markdown_mismatches[:sample_limit],
            "source_hash_mismatches": source_hash_mismatches[:sample_limit],
            "fts_mismatches": fts_mismatches[:sample_limit],
            "missing_source_records": missing_source_records[:sample_limit],
            "extra_source_records": extra_source_records[:sample_limit],
            "baseline_missing_page_text": baseline_corpus["missing_page_text"][:sample_limit],
            "candidate_missing_page_text": candidate_corpus["missing_page_text"][:sample_limit],
        },
    }
    blocking_counts = (
        report["missing_key_count"],
        report["extra_key_count"],
        report["field_mismatch_count"],
        report["markdown_mismatch_count"],
        report["source_hash_mismatch_count"],
        report["fts_mismatch_count"],
        report["missing_source_record_count"],
        report["extra_source_record_count"],
        report["sqlite"]["baseline"]["missing_page_text_count"],
        report["sqlite"]["candidate"]["missing_page_text_count"],
    )
    report["passed"] = all(count == 0 for count in blocking_counts)
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Compare two SQLite-backed Unity documentation corpora for logical equivalence."
    )
    parser.add_argument("--source", help="Optional original documentation root containing Manual and ScriptReference.")
    parser.add_argument("--baseline", required=True, help="Baseline corpus directory containing docs.sqlite.")
    parser.add_argument("--candidate", required=True, help="Candidate corpus directory containing docs.sqlite.")
    parser.add_argument("--output", help="Optional JSON report path.")
    parser.add_argument("--sample-limit", type=int, default=20)
    args = parser.parse_args(argv)

    source = Path(args.source).resolve() if args.source else None
    baseline = Path(args.baseline).resolve()
    candidate = Path(args.candidate).resolve()
    try:
        report = compare(source, baseline, candidate, args.sample_limit)
    except (OSError, sqlite3.Error, ValueError) as exc:
        parser.error(str(exc))
    text = json.dumps(report, indent=2, ensure_ascii=True) + "\n"
    if args.output:
        Path(args.output).resolve().write_text(text, encoding="utf-8")
    print(text, end="")
    raise SystemExit(0 if report["passed"] else 1)


if __name__ == "__main__":
    main()
