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
# 账目（#46，2026-08-09 起）：
#   - merge 成功后自动 `git rev-parse official/main > UPSTREAM_BASE`，并入 merge 提交；
#     若 merge 为快进（无 merge 提交）则单独提交。UPSTREAM_BASE 语义 = 最近一次 sync 的官方 HEAD。
#   - 校验 `git merge-base HEAD official/main == official/main`（真落后 0），不一致输出告警。
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

# 6) 账目校验：merge-base 必须等于 official/main HEAD（真落后 0）
OFFICIAL_HEAD=$(git rev-parse "$OFFICIAL/main")
if [ "$(git merge-base HEAD "$OFFICIAL/main")" != "$OFFICIAL_HEAD" ]; then
  echo "账目告警：merge-base != official/main HEAD，sync 流程异常（不应出现），请人工核查" >&2
else
  echo "  账目校验：merge-base == official/main（真落后 0）"
fi

# 7) 自动 bump UPSTREAM_BASE（#46，并入 merge 提交；快进合并则单独提交）
git rev-parse "$OFFICIAL/main" > UPSTREAM_BASE
if git diff --quiet -- UPSTREAM_BASE; then
  echo "  UPSTREAM_BASE 无需更新（已等于官方 HEAD）"
else
  git add UPSTREAM_BASE
  if git rev-parse -q --verify HEAD^2 >/dev/null 2>&1; then
    git commit --amend --no-edit
  else
    git commit -m "docs: bump UPSTREAM_BASE to $(git rev-parse --short "$OFFICIAL/main")"
  fi
  echo "  UPSTREAM_BASE → $(cat UPSTREAM_BASE)"
fi

if [ "${1:-}" = "--push-pr" ]; then
  git push "$PUBLIC" "$BRANCH"
  gh pr create -R "$(git remote get-url "$PUBLIC" | sed -E 's#https://github.com/##; s#\.git$##')" \
    --base main --head "$BRANCH" --title "sync: upstream main（${STAMP} 快照）" \
    --body "merge 式同步 official/main，fork 隔离面见 docs/FORK_SURFACE.md；红线校验已过（controller/relay.go hook 完好）"
fi
