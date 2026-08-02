#!/usr/bin/env python3
"""local-path-guard: fail if a tracked file carries an absolute filesystem path
out of somebody's machine.

The sibling agent-config-guard matches path NAMES. This class hides in file
CONTENT, which is how a validation-artifact directory (Temp/M117) shipped a
machine name, account names, and local paths past a green CI in 2026-08.

Absolute paths published on purpose are listed as prefixes in
.publish-path-allowlist, one per line. Comparison is done on forward slashes
with runs of separators collapsed, so a single entry covers the \\ and \\\\
spellings of the same path too.

Exit 0 clean, 1 with violations listed on stderr.
"""

import re
import subprocess
import sys
from pathlib import Path

ALLOWLIST = Path(".publish-path-allowlist")

# Drive-absolute (X:\<dir>\<dir>, either slash, singled or doubled) and POSIX
# per-user home paths. Two guards against false positives: the lookbehind keeps
# "https://host/path" and prose like "moves:\nNext" from reading as drive
# letters, and the drive form needs two or more segments so a format string
# like "%d:\n" cannot match. Cost of that second rule: a bare drive plus one
# segment slips through. Everything that identifies a machine - a home dir, a
# nested project root - has depth.
#
# Examples in this file are written with <dir> placeholders on purpose: the
# guard scans every tracked file including itself, and a literal specimen path
# here would be a self-trip. It reads the working tree but only for files git
# tracks, so verify a change from a fresh clone - an untracked new file is
# invisible to it.
PATTERN = re.compile(
    r"(?<![A-Za-z0-9_])"
    r"(?:[A-Za-z]:[\\/]{1,2}[A-Za-z0-9_.\-]+(?:[\\/]{1,2}[A-Za-z0-9_.\-]+)+"
    r"|/(?:home|Users)/[A-Za-z0-9_.\-]+)"
)


def normalize(path):
    return re.sub(r"/{2,}", "/", path.replace("\\", "/"))


def load_allowlist():
    if not ALLOWLIST.exists():
        return []
    out = []
    for line in ALLOWLIST.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if line and not line.startswith("#"):
            out.append(normalize(line))
    return out


def tracked_files():
    raw = subprocess.run(
        ["git", "ls-files", "-z"], capture_output=True, check=True
    ).stdout
    return [f for f in raw.decode("utf-8").split("\0") if f]


def main():
    allowed = load_allowlist()
    violations = []

    for name in tracked_files():
        try:
            data = Path(name).read_bytes()
        except OSError:
            continue
        if b"\0" in data:
            continue  # binary
        text = data.decode("utf-8", errors="replace")
        for lineno, line in enumerate(text.splitlines(), 1):
            for hit in PATTERN.findall(line):
                norm = normalize(hit)
                if any(norm.startswith(a) for a in allowed):
                    continue
                violations.append((name, lineno, hit))

    if not violations:
        print("local-path-guard: clean.")
        return 0

    print("local-path-guard: absolute local path(s) in tracked content:", file=sys.stderr)
    seen = set()
    for name, lineno, hit in violations:
        key = (name, hit)
        if key in seen:
            continue
        seen.add(key)
        print(f"  {name}:{lineno}: {hit}", file=sys.stderr)
    print(
        f"If published on purpose, add the path prefix to {ALLOWLIST} (one per line).",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
