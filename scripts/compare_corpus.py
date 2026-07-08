#!/usr/bin/env python3
import argparse
import hashlib
import json
import re
import sqlite3
from pathlib import Path


STRICT_FIELDS = [
    "page_key",
    "section",
    "page_id",
    "title",
    "source_rel",
    "md_rel",
    "canonical_url",
    "source_sha256",
    "source_bytes",
]


def sha256_bytes(data):
    return hashlib.sha256(data).hexdigest()


def load_pages(corpus):
    pages_path = corpus / "pages.jsonl"
    records = {}
    if not pages_path.exists():
        return records
    with pages_path.open("r", encoding="utf-8") as handle:
        for line_number, line in enumerate(handle, 1):
            if not line.strip():
                continue
            record = json.loads(line)
            key = record.get("page_key") or f"{record.get('section')}/{record.get('page_id')}"
            record["_line"] = line_number
            records[key] = record
    return records


def collect_source_rels(source):
    if not source:
        return set()
    rels = set()
    for section in ["Manual", "ScriptReference"]:
        root = source / section
        if root.exists():
            rels.update(path.relative_to(source).as_posix() for path in root.rglob("*.html"))
    return rels


def normalize_text(value):
    value = value.replace("\r\n", "\n").replace("\r", "\n")
    value = re.sub(r"[ \t\f\v]+", " ", value)
    value = re.sub(r" *\n *", "\n", value)
    value = re.sub(r"\n{3,}", "\n\n", value)
    return value.strip()


def markdown_body(path):
    text = path.read_text(encoding="utf-8", errors="replace")
    marker = "\n## Content\n\n"
    if marker not in text:
        return normalize_text(text)
    body = text.split(marker, 1)[1]
    links_marker = "\n## Content Links"
    if links_marker in body:
        body = body.split(links_marker, 1)[0]
    return normalize_text(body)


def sqlite_page_count(corpus):
    db_path = corpus / "docs.sqlite"
    if not db_path.exists():
        return {"exists": False}
    conn = sqlite3.connect(db_path)
    try:
        pages = conn.execute("SELECT COUNT(*) FROM pages").fetchone()[0]
        try:
            fts = conn.execute("SELECT COUNT(*) FROM pages_fts").fetchone()[0]
            fts5 = True
        except sqlite3.OperationalError:
            fts = None
            fts5 = False
    finally:
        conn.close()
    return {"exists": True, "pages": pages, "fts5": fts5, "pages_fts": fts}


def compare(source, baseline, candidate, sample_limit):
    source_rels = collect_source_rels(source)
    baseline_pages = load_pages(baseline)
    candidate_pages = load_pages(candidate)
    baseline_keys = set(baseline_pages)
    candidate_keys = set(candidate_pages)
    missing_keys = sorted(baseline_keys - candidate_keys)
    extra_keys = sorted(candidate_keys - baseline_keys)
    field_mismatches = []
    markdown_missing = []
    text_hash_mismatches = []
    body_mismatches = []
    source_hash_mismatches = []

    for key in sorted(baseline_keys & candidate_keys):
        base = baseline_pages[key]
        cand = candidate_pages[key]
        for field in STRICT_FIELDS:
            if base.get(field) != cand.get(field):
                field_mismatches.append(
                    {"page_key": key, "field": field, "baseline": base.get(field), "candidate": cand.get(field)}
                )
                break
        base_md = baseline / base.get("md_rel", "")
        cand_md = candidate / cand.get("md_rel", "")
        if not base_md.exists() or not cand_md.exists():
            markdown_missing.append(
                {"page_key": key, "baseline_exists": base_md.exists(), "candidate_exists": cand_md.exists()}
            )
            continue
        if base.get("text_sha256") != cand.get("text_sha256"):
            text_hash_mismatches.append({"page_key": key, "baseline": base.get("text_sha256"), "candidate": cand.get("text_sha256")})
            if markdown_body(base_md) != markdown_body(cand_md):
                body_mismatches.append({"page_key": key})
        if source:
            source_path = source / base["source_rel"]
            if source_path.exists():
                actual_hash = sha256_bytes(source_path.read_bytes())
                if actual_hash != cand.get("source_sha256"):
                    source_hash_mismatches.append(
                        {"page_key": key, "source_rel": base["source_rel"], "expected": actual_hash, "candidate": cand.get("source_sha256")}
                    )

    candidate_source_rels = {record["source_rel"] for record in candidate_pages.values() if "source_rel" in record}
    missing_source_records = sorted(source_rels - candidate_source_rels) if source else []
    extra_source_records = sorted(candidate_source_rels - source_rels) if source else []
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
        "markdown_missing_count": len(markdown_missing),
        "text_hash_mismatch_count": len(text_hash_mismatches),
        "body_mismatch_count": len(body_mismatches),
        "source_hash_mismatch_count": len(source_hash_mismatches),
        "missing_source_record_count": len(missing_source_records),
        "extra_source_record_count": len(extra_source_records),
        "sqlite": {
            "baseline": sqlite_page_count(baseline),
            "candidate": sqlite_page_count(candidate),
        },
        "samples": {
            "missing_keys": missing_keys[:sample_limit],
            "extra_keys": extra_keys[:sample_limit],
            "field_mismatches": field_mismatches[:sample_limit],
            "markdown_missing": markdown_missing[:sample_limit],
            "text_hash_mismatches": text_hash_mismatches[:sample_limit],
            "body_mismatches": body_mismatches[:sample_limit],
            "source_hash_mismatches": source_hash_mismatches[:sample_limit],
            "missing_source_records": missing_source_records[:sample_limit],
            "extra_source_records": extra_source_records[:sample_limit],
        },
    }
    blocking_counts = [
        report["missing_key_count"],
        report["extra_key_count"],
        report["field_mismatch_count"],
        report["markdown_missing_count"],
        report["body_mismatch_count"],
        report["source_hash_mismatch_count"],
        report["missing_source_record_count"],
        report["extra_source_record_count"],
    ]
    report["passed"] = all(count == 0 for count in blocking_counts)
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description="Compare two Unity documentation agent corpora for logical equivalence.")
    parser.add_argument("--source", help="Optional original Unity documentation root containing Manual and ScriptReference.")
    parser.add_argument("--baseline", required=True, help="Baseline corpus directory, normally Python-generated.")
    parser.add_argument("--candidate", required=True, help="Candidate corpus directory, normally native-generated.")
    parser.add_argument("--output", help="Optional JSON report path.")
    parser.add_argument("--sample-limit", type=int, default=20)
    parser.add_argument("--strict-body", action="store_true", help="Fail when normalized Markdown bodies differ.")
    args = parser.parse_args(argv)

    source = Path(args.source).resolve() if args.source else None
    baseline = Path(args.baseline).resolve()
    candidate = Path(args.candidate).resolve()
    report = compare(source, baseline, candidate, args.sample_limit)
    if not args.strict_body and report["body_mismatch_count"]:
        report["passed"] = (
            report["missing_key_count"] == 0
            and report["extra_key_count"] == 0
            and report["field_mismatch_count"] == 0
            and report["markdown_missing_count"] == 0
            and report["source_hash_mismatch_count"] == 0
            and report["missing_source_record_count"] == 0
            and report["extra_source_record_count"] == 0
        )
        report["body_mismatch_note"] = "Body differences are diagnostic unless --strict-body is used; inspect samples and retrieval benchmarks for semantic impact."
    text = json.dumps(report, indent=2, ensure_ascii=False) + "\n"
    if args.output:
        Path(args.output).resolve().write_text(text, encoding="utf-8")
    print(text, end="")
    raise SystemExit(0 if report["passed"] else 1)


if __name__ == "__main__":
    main()
