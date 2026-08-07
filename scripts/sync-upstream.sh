#!/usr/bin/env bash
# 追上游同步 SOP（merge 式，保留上游 SHA，账目真实）
#
# 用法：
#   bash scripts/sync-upstream.sh            # 只做本地合并（含红线校验，冲突即中止）
#   bash scripts/sync-upstream.sh --push-pr  # 合并后推送并创建 PR
#
# 为什么用 merge 不用 cherry-pick：cherry-pick 会产生"内容已在但 SHA
# 对不上"的假落后（2026-08-06 实测），merge 保留官方 commit SHA，
# `git rev-list public/main..official/main` 的账目永远真实。
#
# fork 红线（docs/FORK_SURFACE.md）：合并后 controller/relay.go 必须保留
# vision relay hook（4 行）；本脚本合并后自动校验，失败即中止。
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

STAMP=$(date +%Y%m%d)
BRANCH="sync/upstream-${STAMP}"
PUBLIC="${REMOTE_PUBLIC:-public}"
OFFICIAL="${REMOTE_OFFICIAL:-official}"

# 1) 拉取两端 main
git fetch "$OFFICIAL" main
git fetch "$PUBLIC" main

# 2) 工作树必须干净
if [ -n "$(git status --porcelain)" ]; then
  echo "工作树不干净，中止（先提交或 stash）" >&2
  exit 1
fi

# 3) 从 public/main 开 sync 分支
git checkout -B "$BRANCH" "$PUBLIC/main"

# 4) 合并官方 main（冲突交给人工，成功后由 CI 门禁）
if ! git merge "$OFFICIAL/main" --no-edit; then
  echo "合并冲突：手工解决后 git merge --continue" >&2
  exit 2
fi

# 5) fork 红线校验：controller/relay.go 的 vision relay hook 必须保留
HOOKS=$(git grep -c "vision_relay" -- controller/relay.go | cut -d: -f2 || true)
if [ "${HOOKS:-0}" -lt 1 ]; then
  echo "红线校验失败：controller/relay.go 丢失 vision relay hook" >&2
  exit 3
fi

echo "同步就绪：$BRANCH"
echo "  合并提交：$(git log --oneline "$PUBLIC/main..$BRANCH" | wc -l)"
echo "  落后账目：$(git rev-list --count "$BRANCH..$OFFICIAL/main") 领先：$(git rev-list --count "$OFFICIAL/main..$BRANCH")"

if [ "${1:-}" = "--push-pr" ]; then
  git push "$PUBLIC" "$BRANCH"
  gh pr create -R "$(git remote get-url "$PUBLIC" | sed -E 's#https://github.com/##; s#\.git$##')" \
    --base main --head "$BRANCH" --title "sync: upstream main（${STAMP} 快照）" \
    --body "merge 式同步 official/main，fork 隔离面见 docs/FORK_SURFACE.md；红线校验已过（controller/relay.go hook 完好）"
fi
