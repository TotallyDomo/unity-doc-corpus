#!/usr/bin/env sh
# Build the two Go binaries into bin/. POSIX-sh peer of build.ps1 for Unix-like
# shells (Linux, macOS, Git Bash on Windows). The canonical path is still the two
# `go build` commands in the README quickstart; this wrapper adds the same
# conveniences build.ps1 has: quiet-on-success (go build chatter otherwise
# re-enters agent context on every later turn), symbol-stripped output (-s -w),
# and a post-build existence check. Set VERBOSE=1 to see go build output on
# success too.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
out_dir="$repo_root/bin"
go_root="$repo_root/go"

mkdir -p "$out_dir"

# Quiet on success: show go build output only when a step fails (or under VERBOSE).
run_go() {
    label=$1
    shift
    if ! out=$(cd "$go_root" && go "$@" 2>&1); then
        printf '%s\n' "$out" >&2
        echo "$label failed" >&2
        exit 1
    fi
    if [ -n "${VERBOSE:-}" ]; then
        printf '%s\n' "$out"
    fi
}

# Go names the binaries itself and appends .exe on Windows; the trailing slash on
# -o makes it write into bin/ under those names, matching the README quickstart.
run_go "go build" build -trimpath -ldflags "-s -w" -o "$out_dir/" .
run_go "go build (benchmark)" build -trimpath -ldflags "-s -w" -o "$out_dir/" ./cmd/unity-doc-corpus-benchmark

# Confirm each binary landed, tolerating the .exe suffix (Git Bash on Windows).
for base in unity-doc-corpus unity-doc-corpus-benchmark; do
    if [ -f "$out_dir/$base" ]; then
        echo "$out_dir/$base"
    elif [ -f "$out_dir/$base.exe" ]; then
        echo "$out_dir/$base.exe"
    else
        echo "Go build did not produce $out_dir/$base" >&2
        exit 1
    fi
done
