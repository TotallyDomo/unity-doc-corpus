#!/usr/bin/env bash
# agent-config-guard: fail if a tracked agent-config-shaped path (CLAUDE.md,
# AGENTS.md, .claude/, .agents/, MEMORY.md, MCP/settings files) is not
# explicitly allowed. Guards against agent scratch riding a bulk commit into
# a public repo (2026-07-19 Netflix metaflow-nflx-extensions CLAUDE.md leak;
# digest .agents/intake/digests/metaflow-nflx-claude-md.md in AgentWorkspace).
#
# A repo that wants a deliberate user-facing entry (e.g. a reviewed AGENTS.md
# for visiting contributors) lists its exact path in .agent-publish-manifest,
# one per line, blank lines and #-comments ignored, to allow it.
set -euo pipefail

is_agent_config() {
  case "$1" in
    CLAUDE.md|CLAUDE.local.md|AGENTS.md|AGENTS_EXTERNAL.md|MEMORY.md|.mcp.json) return 0 ;;
    .claude/*|.agents/*) return 0 ;;
    *) return 1 ;;
  esac
}

manifest=".agent-publish-manifest"
allowed=()
if [[ -f "$manifest" ]]; then
  while IFS= read -r line; do
    [[ -z "$line" || "$line" == \#* ]] && continue
    allowed+=("$line")
  done < "$manifest"
fi

is_allowed() {
  local f="$1" a
  for a in ${allowed[@]+"${allowed[@]}"}; do
    [[ "$f" == "$a" ]] && return 0
  done
  return 1
}

violations=()
while IFS= read -r f; do
  if is_agent_config "$f" && ! is_allowed "$f"; then
    violations+=("$f")
  fi
done < <(git ls-files)

if (( ${#violations[@]} > 0 )); then
  echo "agent-config-guard: un-manifested agent-config path(s) found:" >&2
  printf '  %s\n' "${violations[@]}" >&2
  echo "If deliberate, add the exact path to $manifest (one per line)." >&2
  exit 1
fi

echo "agent-config-guard: clean."
