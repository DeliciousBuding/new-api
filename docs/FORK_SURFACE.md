# FORK_SURFACE — DeliciousBuding/new-api 补丁面清单

> 本文档是 fork 治理的长期 SSOT：每个产品线的自有目录、上游覆盖层、重放顺序与验收命令。
> 配套文件：`UPSTREAM_BASE`（基线 SHA）、`.github/workflows/upstream-check.yml`（落后检测）、
> `.github/scripts/verify-td-release.sh`（release 验证）、`docs/session-reports/`（会话记录）。
> 最后更新：2026-08-03

## 0. 当前状态快照

| 指标 | 值 |
|------|-----|
| UPSTREAM_BASE | `9724ef1b248a436ea47270bb5b394a0fdb013a6c` |
| 官方 main HEAD | `0ab02020`（落后 2 个实质 commit，均未 patch-equivalent） |
| fork 本地提交数 | ~102（自 BASE） |
| fork 新增文件 | 120 |
| fork 修改的上游文件 | 66 |
| 与官方 BASE..main 的交集文件 | 20（冲突热点，见 §3） |

官方待引入的 2 个 commit：

| commit | 内容 | 与本地交集 |
|--------|------|-----------|
| `0ab02020` Feat/auto group (#6590) | token auto-groups（controller/token.go +132、model.go、auth middleware、i18n） | `constant/context_key.go`（+1 行本地 context key）、keys/system-settings 前端、i18n locales |
| `bd585d78e` fix(aws): cancel Bedrock on disconnect (#6589) | relay-aws 取消传播 + billing_usage | 无直接文件交集（relay-aws.go、billing_usage.go 本地未改） |

`git cherry` 确认官方 BASE..main 其余 commit（含 #6518/#6570）在 fork 中已有 patch-equivalent 重放，同步时不可按 SHA 机械重复。

## 1. 产品线清单（重放顺序即编号）

### P1 · Fork release / Docker / CI
- **自有目录**：`.github/`、`Dockerfile`、`VERSION`、`NOTICE`、`THIRD-PARTY-LICENSES.md`、`AGENTS.md`、`UPSTREAM_BASE`
- **上游覆盖**：ci.yml / docker-build.yml / release.yml（self-hosted sgp2 runner、GHCR 个人 owner、node 22 钉版）；pr-check.yml 已删（2026-08-03，外部审核建议）
- **上游等价**：无（纯 fork 侧）
- **重放**：最先（与上游零交集）

### P2 · Subscription / 自有 billing
- **上游覆盖**：`service/billing_session.go`、`service/tiered_settle.go`(+test)、`service/quota.go`、`service/text_quota.go`、`pkg/billingexpr/types.go`(+expr.md)、`model/option.go`、`model/log.go`(+test)
- **冲突热点**：billing_session/tiered_settle 与官方 #6590 的 model 层邻近；pkg/billingexpr 与官方 expr 文档
- **上游等价**：部分（计费修复在上游演进中已吸收，重放前逐个 `git cherry` 核）

### P3 · Logging / client-profile / GeoIP
- **上游覆盖**：`relay/common/relay_info.go`（+observer 无关的 profile 字段）、`controller/log.go`、`model/log.go`、`service/log_info_generate.go`、`web/src/features/usage-logs/*`（5 文件）
- **冲突热点**：relay_info.go 与官方热点；usage-logs 前端
- **上游等价**：无

### P4 · Relay Observability（observer 核心）
- **自有目录**：`pkg/relay_observer/**`（120 新增文件的核心）、`controller/relay_observer*.go`、`web/src/features/observability/**`
- **上游覆盖**（现状，待 §2 收缩后应 ≤8 文件、每处 ≤1 行）：
  - `main.go`（runtime 创建/Init/注入/Close）
  - `controller/relay.go`（attempt hooks 内联）
  - `service/quota.go` + `service/text_quota.go`（settlement 各 1 行 + TurnUsage 组装 5 行）
  - `router/api-router.go`（路由块内联）
  - `service/relay_observation.go`（当前全部 bridge 逻辑所在）
- **上游等价**：无（私有产品线）
- **重放**：见 §2 seam 方案；分 4 个 topic：core+storage → lifecycle seam+bridge → Root API → frontend

### P5 · Dashboard / cache / keys UI
- **上游覆盖**：`web/src/features/dashboard/*`（3）、`web/src/features/keys/*`（3）、`web/src/features/system-settings/*`（4）、`web/src/features/channels/*`（2）、`web/src/features/auth/*`（2）、`web/src/features/profile/*`（2）
- **冲突热点**：keys api/columns、system-settings/types 与官方 #6590 直接重叠
- **上游等价**：无

### P6 · 前端基建 overlay（extension 层）
- **上游覆盖**：`web/src/main.tsx`、`web/src/hooks/*`、`web/src/assets/brand-icons`、`web/src/routeTree.gen.ts`（生成文件，永不手工合并）、`web/package.json`、`web/bun.lock`
- **计划**（外部审核建议）：新增 `web/src/extensions/{navigation,sections,i18n}.ts` 编译期静态 overlay；上游文件只保留一次性 spread

### P7 · i18n
- **上游覆盖**：`web/src/i18n/locales/*`（en/zh/ja/fr/ru/vi/zh-TW 9 文件）
- **冲突热点**：6 个 locale 与官方重叠；上游每次整理 locale 都会撞
- **计划**（外部审核建议）：observer 翻译迁到 `web/src/features/observability/i18n/{en,zh}.json` + `addResourceBundle` 合入；其余语言 fallback 英文。此后自有 key 不再碰原生 locale 文件

### P8 · Vision-relay（另一会话在途）
- **自有目录**：`relay/vision/**`、`setting/model_setting/vision*.go`（未提交至 main，feat/vision-relay 分支）
- **上游覆盖**：`constant/context_key.go`（+1）
- **状态**：设计文档 `docs/plan/vision-relay.md` 已评审（v0.2），代码在途

## 2. Relay Observability 接缝收缩方案（W0 设计）

目标依赖方向（单一消费者、类型化、默认 no-op，不造事件总线）：

```text
NewAPI 原生 relay / billing
   │ 5~8 个一行 hook（无 observer 业务逻辑）
   ▼
service/relay_lifecycle.go        ← 中立生命周期接口 + no-op 默认
   ▼
service/relay_observer_bridge.go  ← observer 适配层（DTO 转换/运行时持有）
   ▼
pkg/relay_observer/               ← 核心（不反向依赖 gin/service/controller）
```

硬规则：
1. `pkg/relay_observer` 不 import gin / service / controller。
2. 原生上游文件不直接 import `pkg/relay_observer`（通过 bridge）。
3. 原生文件只放一行 `NotifyRelay*` 调用；event 构造、BodyStorage、HMAC、runtime 判断全进 bridge。
4. 删 observer = 删自有目录 + 移除 5~8 行接线。
5. PG 特例限制在 `pkg/relay_observer/store_pg.go` 与 migrations，不污染 `model/`。
6. `relay/common/RelayInfo` 不新增 observer 专用字段。

具体动作：
- 新建 `service/relay_lifecycle.go`：`RelayLifecycleSink`（AttemptBegin/AttemptEnd/Settled/Failed）+ no-op 默认 + `NotifyRelay*` 转发。
- 新建 `service/relay_observer_bridge.go`：observer 运行时持有、settlement DTO 转换、query surface 窄接口。
- `controller/relay.go`：attempt 循环内 3 处 → `service.NotifyRelayAttemptBegin/End/Failed`。
- `service/quota.go` / `service/text_quota.go`：5 行 TurnUsage 组装 → `NotifyRelaySettled(ctx, info, usage, quota)` 一行。
- `main.go`：`service.InitOptionalRelayObserver()` + `service.CloseOptionalRelayObserver(ctx)` 两行。
- `router/relay_observer_routes.go`：路由块移出 api-router.go，`api-router.go` 剩 `registerRelayObserverRoutes(apiRouter)`（沿用 registerChannelRoutes 模式）。

## 3. 冲突热点（B∩C，20 文件）

按官方 #6590/#6589 引入时的预计冲突强度排序：

| 文件 | 本地改动性质 | 官方改动 | 冲突强度 |
|------|-------------|---------|---------|
| `controller/relay.go` | observer hooks 内联 3 处 | 未直接改（邻近） | 中 |
| `service/tiered_settle.go`(+test) | 自有 billing | #6590 计费邻近 | 中 |
| `service/billing_session.go` | 自有 billing | 邻近 | 中 |
| `constant/context_key.go` | +1（vision/observer） | #6590 +1 | 低（追加即可） |
| `pkg/billingexpr/*` | 自有扩展 | 上游演进 | 低 |
| `web/src/features/keys/*` | 自有 UI | #6590 token 控制面 | 中高 |
| `web/src/features/system-settings/types.ts` | 自有扩展 | #6590 | 中 |
| `web/src/i18n/locales/*` (6) | 自有 key | #6590 新 key | 中（§1-P7 迁移后归零） |
| `relay/common/relay_info.go` | profile 字段 | 未直接改 | 低 |
| `router/api-router.go` | observer 路由块 | 未直接改 | 低（§2 提取后归零） |
| `model/option.go` | 自有选项 | 未直接改 | 低 |

## 4. 影子重建流程（下一轮执行）

```bash
# W1 基线：从官方 HEAD 建重建分支（不 merge 当前 main）
git fetch official
git switch -c rebuild/upstream-20260803 official/main

# W2 按 §1 产品线顺序重放（P1 → P8），每线一个 topic commit：
#   P1 fork-release → P2 billing → P3 logging → P4 observer(4 topics) →
#   P5 dashboard/keys → P6 extensions → P7 i18n → P8 vision
# 重放前对每线 `git cherry` 去重，已被上游吸收的不重放

# W3 冲突面验收
git diff --name-only official/main..HEAD | grep -v '^\(pkg/relay_observer\|web/src/features/observability\|controller/relay_observer\|web/src/extensions\|docs/\|\.github/\|AGENTS\|VERSION\|Dockerfile\|NOTICE\|UPSTREAM_BASE\)' \
  | sort > overlay-remaining.txt   # 预期 ≤8 文件且每文件为 import/注册/单行 hook

# W4 验证：旧 main 归档
git branch archive/pre-upstream-rebuild-20260803 main
# 重建分支：单元 + PG 集成 + 前端构建全绿后再切换
```

## 5. 验收命令（每产品线重放后）

```bash
# 后端
go build ./... && go vet ./pkg/relay_observer/ ./service/ ./controller/ ./router/
go test ./pkg/relay_observer/ -count=1
go test -tags relay_observer_pg_integration ./pkg/relay_observer/ -count=1   # 需 TEST_RELAY_OBSERVER_POSTGRES_DSN

# 前端
cd web && bun install --frozen-lockfile && bun run typecheck && bun test && bun run build

# fork 面
git diff --name-only official/main..HEAD | wc -l   # 趋势只降不升（P7 迁移后 i18n 归零）
```

## 6. 未决风险

- `upstream-check.yml` 仍 `runs-on: ubuntu-latest`——private repo 无 hosted minutes，该检查实际不生效；迁移 sgp2（只 checkout 受信任 main + fetch 官方公开仓库，无 secrets，可安全迁）。
- 上游周更频率高：建议 upstream status 日跑、实际 rebuild 周跑、大功能开发前手动跑。
- vision-relay（P8）与 observer 共用 context_key 文件，重放时注意追加式合并。
