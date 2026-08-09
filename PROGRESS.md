# PROGRESS — 当前 backlog 指针

最后更新：2026-08-09 18:10

> 进度追踪迁移至 GitHub Issues + FORK_SURFACE。本文件只保留跨会话指针，不写单任务进度（避免 stale 拋留）。

## 当前状态

- **主线 main**：含 audit-2026-08 全部修复批次（#65/#62/#63/#61/#60/#66 已合，详见 `docs/FORK_SURFACE.md` §4d）
- **仓库**：public，CI 用 hosted ubuntu-latest
- **ruleset**（#20184444）：精简为 deletion + non_fast_forward + pull_request（2026-08-09，去掉 required_status_checks 阻挡，CI 改 advisory；docs-only PR 不再被 BLOCKED）
- **UPSTREAM_BASE**：`823e26304`（#61 已 bump，落后官方 0，#6674/#6711 待吸收）

## backlog 入口

- **GitHub Issues**：7 个 open，milestone「backlog 2026-Q3」（#2）
  - **stability & perf**：#36 critical（log INSERT 占主池）、#45 medium（DB 池隔离）、#64 high（observer 索引 migration v5）
  - **product hardening**：#40 high（i18n 迁移 96 文件）、#11 high（vision relay 收口）、#1 medium（SDK 分类 bug）
  - **ops 收尾**：#51 medium（runner 注销，workflow 已停用）
- **fork 治理 SSOT**：`docs/FORK_SURFACE.md`（§0 状态快照、§4d 最新合流、§6 未决风险）
- **draft PR**：#34 feat/observability master/detail（VChart 测试需 mock，体量 993 行单独处理）

## 下一轮优先级

1. #36 critical — 同步 log INSERT 占用 open=12 主池（与 #45 DB 池隔离同源，建议一起做）
2. #64 high — observer_turns.content_state 缺索引（待补 migration v5）
3. #40 high — P7 i18n 迁移未落地（96 散落文件 vs ≤8 目标）
4. #11 high — Vision Relay 合并前收口

## 已停用 workflow（2026-08-09）

- `sync-release-to-gitcode.yml` — public 后 gitcode 镜像无意义
- `electron-build.yml` — fork 不发布桌面应用
