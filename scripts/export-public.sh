#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 EMPTY_DESTINATION" >&2
  exit 2
fi

repo_root=$(git rev-parse --show-toplevel)
destination=$1

if [[ -e "$destination" ]] && [[ -n "$(find "$destination" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
  echo "destination must be empty: $destination" >&2
  exit 2
fi

mkdir -p "$destination"

copy_tracked_file() {
  local relative_path=$1
  mkdir -p "$destination/$(dirname "$relative_path")"
  cp "$repo_root/$relative_path" "$destination/$relative_path"
}

public_files=(
  CHANGELOG.md
  COMPONENT_MAP.md
  COMPONENT_MAP.zh-CN.md
  LICENSE
  README.md
  go.mod
  go.sum
  docs/architecture.md
)

for relative_path in "${public_files[@]}"; do
  copy_tracked_file "$relative_path"
done

while IFS= read -r -d '' relative_path; do
  copy_tracked_file "$relative_path"
done < <(git -C "$repo_root" ls-files -z -- '*.go' ':(exclude)**/*_test.go' ':(exclude)*_test.go')

mkdir -p "$destination/.github/workflows"
cp "$repo_root/.public/ci.yml" "$destination/.github/workflows/ci.yml"

if find "$destination" -type f -name '*_test.go' -print -quit | grep -q .; then
  echo "public export contains a test file" >&2
  exit 1
fi

for forbidden_path in \
  AGENTS.md \
  ROADMAP.zh-CN.md \
  docs/agent-architecture-discussion.zh-CN.md \
  docs/agent-framework-architecture.zh-CN.md \
  docs/agent-runtime-standard-slots-implementation-plan.zh-CN.md; do
  if [[ -e "$destination/$forbidden_path" ]]; then
    echo "public export contains internal material: $forbidden_path" >&2
    exit 1
  fi
done

if rg -n \
  'agent-architecture-discussion|agent-framework-architecture|agent-runtime-standard-slots-implementation-plan|ROADMAP\.zh-CN|AGENTS\.md' \
  "$destination"; then
  echo "public export contains an internal-document reference" >&2
  exit 1
fi
