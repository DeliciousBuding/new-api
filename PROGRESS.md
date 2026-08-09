# PROGRESS — 当前 backlog 指针

最后更新：2026-08-09 17:30

> 进度追踪已迁移至 GitHub Issues + FORK_SURFACE。本文件只保留跨会话指针，不写单任务进度（避免 stale 拋留）。

## 当前状态

- **主线 main**：含 audit-2026-08 全部修复批次（#65/#62/#63/#61/#60 已合，详见 `docs/FORK_SURFACE.md` §4d）
- **仓库**：public，CI 用 hosted ubuntu-latest
- **UPSTREAM_BASE**：`5c3abffe8`（落后官方 ~2 天，#46 跟踪下次 sync bump）

## backlog 入口

- **GitHub Issues**：`gh issue list --repo DeliciousBuding/new-api --state open`（当前 9 个 open，1 critical / 3 high）
- **fork 治理 SSOT**：`docs/FORK_SURFACE.md`（§0 状态快照、§4d 最新合流、§6 未决风险）
- **draft PR**：#34 feat/observability master/detail（VChart 测试需 mock，体量 993 行单独处理）

## 下一轮优先级（按 issue severity）

1. #36 critical — 同步 log INSERT 占用 open=12 主池
2. #64 high — observer_turns.content_state 缺索引（待补 migration v5）
3. #40 high — P7 i18n 迁移未落地（96 散落文件 vs ≤8 目标）
4. #11 high — Vision Relay 合并前收口
