# BACKLOG — 设计 debt 与待办归档

最后更新：2026-08-10 15:30

> 本文档归档已关闭 issue 的根因、建议与下一轮推进路径。GitHub Issues 面保持为 0（便于治理），真实 code debt 落此明确 handoff（neat-freak 规则：不藏 TODO，落到文档）。
> 优先级沿用原 issue severity；推进时按需重开 issue 或直接走 PR。

## 1. Stability & Performance（DB / observer 热路径）

### #36 [critical] 同步 log INSERT 占用主池（SQL_MAX_OPEN_CONNS 默认 1000）
- **根因**：`model/log.go` 的 `createLog`（line 127-129）同步 `LOG_DB.Create(log)` 在 relay 热路径占用主连接池，写吞吐瓶颈。
- **建议**：log 写入改异步（channel + batch flush）或独立连接池；与 #45 DB 池隔离同源，建议一起做。
- **原生设计约束**：`model/log.go` 是上游覆盖文件（P3），改动需走 FORK_SURFACE §3 冲突热点评估；优先用 fork 侧 wrapper（service/log_*.go）而非直改 model。

### #45 [medium] DB 池无隔离——relay 写与 admin 聚合共享池
- **根因**：`model/main.go` 单池（`SQL_MAX_OPEN_CONNS` 默认 1000，line 194/238），relay 写与 30-41s admin 聚合争抢连接；无 read replica。
- **建议**：双池（relay 热路径短事务池 + admin 聚合大查询池）或 read replica；或 admin 聚合走单独只读连接。
- **原生设计约束**：`model/main.go` 是上游覆盖文件，双池改动影响面大；评估是否上游已有同类演进。

### #64 [high] observer_turns.content_state 缺索引（生产手工修复，待补 migration v5）
- **根因**：`pkg/relay_observer` 的 overview 查询在 `observer_turns.content_state` 列全表扫，生产已手工加索引但 migration 未提交。
- **建议**：补 `pkg/relay_observer/migrations/005_*.up.sql` 加索引；migration v5。
- **原生设计约束**：migration 在 fork 自有目录（§2 接缝边界），零上游冲突。

## 2. Product Hardening

### #40 [high] P7 i18n 迁移完全未落地（96 散落文件 vs ≤8 目标）
- **根因**：FORK_SURFACE P7 计划把 observer 翻译迁到 `web/src/features/observability/i18n/{en,zh}.json` + `addResourceBundle`，未落地；96 个 locale 散落 key 与上游重叠。
- **建议**：按 P7 计划迁移，目标是自有 key 不再碰原生 locale 文件（≤8 overlay）。
- **原生设计约束**：迁移到 feature-local i18n 是 fork 侧改动，降低与上游 locale 冲突（P7 本就为此设计）。

### #11 [high] Vision Relay 合并前收口：净化分支、封堵递归绕过与请求级鉴权熔断
- **根因**：vision relay（P8）的请求级鉴权熔断与递归绕过封堵需收口。
- **建议**：`service/vision_relay.go` 加请求级熔断（连续失败阈值）+ 递归调用守卫。
- **原生设计约束**：vision relay 是 fork 自有产品线（P8），`controller/relay.go` 单点钩子是唯一上游覆盖，改动在自有目录内。

### #1 [medium] Anthropic SDK requests misclassified as openai_sdk in client_profile
- **根因**：`service/log_info_generate.go` 的 `DetectClientProfile` 函数（line 92）把 Anthropic SDK 请求兜底误判为 `openai_sdk`（line 237/317 默认归 openai_sdk，Anthropic UA 未命中 `claude_cli` 检测条件时兜底）。
- **建议**：`DetectClientProfile` 分类逻辑加 Anthropic SDK UA/header 识别，避免兜底到 `openai_sdk`。
- **原生设计约束**：`service/log_info_generate.go` 是上游覆盖文件（P3），仍在 fork diff 中；改动需走 FORK_SURFACE §3 冲突热点评估。

## 3. Ops 收尾

### #51 [medium] us1-newapi runner 注销
- **进展**：① sync-release-to-gitcode.yml 已**删除**（PR #72，public 仓库后 gitcode 镜像无意义）；② electron-build.yml 保留但仅 workflow_dispatch 手动触发（不自动运行）。
- **剩余**：注销 us1 上的 `wsl-newapi` runner（需 us1 SSH 操作，CI 已迁 hosted 不再依赖）。
- **执行**：`gh runner delete` 或 us1 上 `./config.sh remove`。
