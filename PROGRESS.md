# PROGRESS — 当前 backlog 指针

最后更新：2026-08-10 14:30

> GitHub Issues/PR 面保持为 0（便于治理），真实 code debt 落到 `docs/BACKLOG.md` 明确 handoff。
> neat-freak 规则：不藏 TODO，落到文档；推进时按需重开 issue 或直接走 PR。

## 当前状态

- **主线 main**：含 audit-2026-08 全部修复批次 + 2026-08-10 上游 sync 8 commit（#65/#62/#63/#61/#60/#66/#67/#71/#72/#73/#74/#75 已合，详见 `docs/FORK_SURFACE.md` §4d-§4e）
- **仓库**：public，CI 用 hosted ubuntu-latest
- **ruleset**（#20184444）：deletion + non_fast_forward + pull_request（CI advisory，docs PR 不被 BLOCKED）
- **UPSTREAM_BASE**：`9c97e78ac`（#74 sync 已 bump，落后官方 0）
- **open PR / issue**：0 / 0

## backlog 入口

- **`docs/BACKLOG.md`** — 7 个已归档设计 debt，按优先级分 3 组：
  1. Stability & Perf：#36 critical（log INSERT 占主池）、#45 medium（DB 池隔离）、#64 high（observer 索引）
  2. Product Hardening：#40 high（i18n 迁移）、#11 high（vision relay 收口）、#1 medium（SDK 分类）
  3. Ops：#51 medium（runner 注销，workflow 已删 PR #72）
- **fork 治理 SSOT**：`docs/FORK_SURFACE.md`（§0 状态、§4e 合流、§6 风险）

## 下一轮优先级

1. #36 critical + #45 — DB 池/log 写入（同源，一起做）
2. #64 high — observer 索引 migration v5（零上游冲突）
3. #40 high — P7 i18n 迁移
4. #11 high — Vision Relay 收口

## workflow 状态

- `sync-release-to-gitcode.yml` — 已**删除**（PR #72，public 仓库后 gitcode 镜像无意义）
- `electron-build.yml` — 保留 workflow_dispatch 手动触发（不自动运行）
