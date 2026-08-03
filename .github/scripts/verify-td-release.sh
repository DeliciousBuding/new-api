#!/usr/bin/env bash
set -euo pipefail

tag="${1:-}"
manifest="${2:-tokendance-release-source.json}"

if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-td-[0-9]{8}\.[0-9]+$ ]]; then
  echo "::error::Invalid TokenDance release tag: $tag"
  exit 1
fi

version="$(tr -d '\r\n' < VERSION)"
if [[ "$version" != "$tag" ]]; then
  echo "::error::VERSION '$version' does not match tag '$tag'"
  exit 1
fi

base="$(tr -d '\r\n' < UPSTREAM_BASE)"
if [[ ! "$base" =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::UPSTREAM_BASE must contain one full lowercase SHA"
  exit 1
fi

git cat-file -e "${base}^{commit}"
head_sha="$(git rev-parse HEAD)"
tag_sha="$(git rev-parse "refs/tags/${tag}^{commit}")"
if [[ "$head_sha" != "$tag_sha" ]]; then
  echo "::error::Checked-out commit '$head_sha' is not tag '$tag' ('$tag_sha')"
  exit 1
fi

if ! git merge-base --is-ancestor "$base" "$head_sha"; then
  echo "::error::UPSTREAM_BASE '$base' is not an ancestor of '$head_sha'"
  exit 1
fi

merge_commits="$(git rev-list --min-parents=2 "${base}..${head_sha}")"
if [[ -n "$merge_commits" ]]; then
  echo "::error::Release topic stack contains merge commits"
  printf '%s\n' "$merge_commits"
  exit 1
fi

topic_count="$(git rev-list --count "${base}..${head_sha}")"
if (( topic_count < 1 || topic_count > 20 )); then
  echo "::error::Release topic stack count '$topic_count' is outside the audited 1..20 range"
  exit 1
fi

python3 - "$manifest" "$tag" "$head_sha" "$base" "$topic_count" <<'PY'
import json
import pathlib
import sys

path, tag, source, base, topic_count = sys.argv[1:]
payload = {
    "schema": "tokendance-gateway-release/v1",
    "tag": tag,
    "source_sha": source,
    "upstream_base_sha": base,
    "topic_commit_count": int(topic_count),
}
pathlib.Path(path).write_text(
    json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8"
)
PY

echo "Verified $tag at $head_sha from upstream base $base ($topic_count commits)."
