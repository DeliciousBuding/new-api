# FORK_SURFACE — DeliciousBuding/new-api 补丁面清单

> 本文档是 fork 治理的长期 SSOT：每个产品线的自有目录、上游覆盖层、重放顺序与验收命令。
> 配套文件：`UPSTREAM_BASE`（基线 SHA）、`.github/workflows/upstream-check.yml`（落后检测）、
> `.github/scripts/verify-td-release.sh`（release 验证）、`docs/session-reports/`（会话记录）。
> 最后更新：2026-08-09 18:50

## 0. 当前状态快照

| 指标 | 值 |
|------|-----|
| 仓库可见性 | **public**（2026-08-09 由 private 转公开，CI 用免费 hosted ubuntu-latest） |
| UPSTREAM_BASE | `823e26304a396854ace30b52b98ec497c2dd9c36`（= 官方 2026-08-09 快照；#46 起语义 = 最近一次 sync 的官方 HEAD，见 §4d） |
| 官方 main HEAD | `823e26304`（已完全同步，**落后 0**；merge-base == official/main == UPSTREAM_BASE） |
| 主线 | `main`（public 远端），合流点见 §4a–§4d |
| fork 分支策略 | 单主线 + topic 分支，功能收口 = 合入 main + 删分支（2026-08-05 起强制） |
| CI runner | `ci.yml` 用 **hosted ubuntu-latest**（PR #65，2026-08-09 迁移，不再依赖本地 WSL runner）；`docker-build.yml` 用 hosted ubuntu-latest（release 构建） |
| 最新发版 tag | `v1.0.0-td-20260809.1`（2026-08-09，含 audit 全批次 + public 化；tag 规范见 §5a） |

### 4a. 2026-08-05 合流记录（observer + vision-relay 收口 + 上游同步）

- **observer（P4）终审修复 6 提交**（typed Responses、identity schema、UI 契约、concurrency race、v4 migration）经 PR #12 合入主线；T5.2 hardening 批次已由整合提交 `0c78fd75c` 先前进入 main，未重复携带
- **vision-relay（P8）** 全部开发（v0.2.1 → v0.2.2 → 终审修复 + 设置 UI）经 PR #12 合入主线
- **DeepSeek relay ci 提交**（us1 runner 迁移，原在 audit 分支）不属于本线，走 `codex/sgp2-deepseek` 线
- **CI runner**：`ci.yml` 用本地 WSL runner（`wsl-newapi`）；`docker-build.yml` 用 hosted ubuntu-latest（release 构建）；us1 上的 `us1-newapi` runner 仍注册但不再被 new-api workflow 引用
- **上游同步（2026-08-05）**：合并官方 #6589（Bedrock 断开取消）+ #6590（token auto-groups，57 文件）零冲突；修复官方 #6590 半成品 bug（`api-key-group-cell` 注释掉 `AutoGroupBadge` 致前端测试 3 失败，官方 CI 不跑测试未暴露）；UPSTREAM_BASE 推进至 `0ab02020`（PR #21）
- **CI runner 迁移（2026-08-05）**：hosted runner 在 private 仓库无步骤失败（billing 层拒绝）→ us1 4 核过载 → 最终 **本地 WSL runner（`wsl-newapi`，28 核）**：预装 go/bun/node 跳过 toolchain 下载（WSL→GitHub 下载不稳），CI 全绿
- 已删除 37 个本地旧分支（observer p1-p5 系列、semantic-selector、vision-relay 三代、fix/p0-* 等），保留 `main`/release 线

官方已同步（2026-08-05）：#6589 Bedrock 断开取消 + #6590 token auto-groups 均已并入 main（合并 commit `539924adb`），UPSTREAM_BASE 已推进至 `0ab02020`，当前落后 0。

### 4b. 2026-08-07 合流记录（上游同步 + 分支清理）

- **上游同步（2026-08-07）**：merge 式合并官方 main 3 commit（`d6b5ce99d` relay #6249 GetBody HTTP/2 重试、`ea4f02101` replay metadata 重构、`0cd9dc85e` 上游 merge 回 fork 的 user/router）**零冲突**（24 files, +1173）；controller/relay.go 的 vision relay hook 红线校验通过（PR #29）
- **同步方式定规**：统一 merge 式（保留上游 SHA，账目真实；cherry-pick 会产生"内容在但 SHA 差"的假落后）。自动化脚本 `scripts/sync-upstream.sh`（fetch → sync/<date> 分支 → merge → 红线校验 → push → PR）
- **分支清理（2026-08-07）**：按"功能收口 = 合入 main + 删分支"策略清掉已合并分支（feat/vision-relay-responses、docs/vision-relay-prod-20260806、fix/vision-relay-hardening、style/gofmt-vision-test、docs/hk3-deploy-plan）+ 废弃分支（rebuild/upstream-20260803、codex/sgp2-deepseek、codex/release-* 本地副本）；远端保留 `main` + `fix/responses-data-uri-prefix`（#28 在途）+ 受保护 `codex/release-*`（仓库规则保护，GH013）
- **响应格式收尾（#28）**：`input_image.data` 容错 data URI 前缀（codex_cli 生产实证），本地 WSL E2E 五形态全绿后提交（详见 `docs/plan/vision-relay.md` §25）

### 4c. 2026-08-07 二次合流记录（true merge 上游 8 commit + 账目修正）

- **true merge（PR #32）**：merge 式合并官方 main 8 commit（`d6b5ce99d` #6249 GetBody HTTP/2 重试、`ea4f02101` replay metadata 重构、`0cd9dc85e` user/router、`1da23d6b3` rate-limit middleware、`c9bc03864` #6632 模型分类、`b941253ae` #6698 测活、`e926e5cac` #6685 兑码精度、`5c3abffe8` CI sync-release）
- **账目修正**：核实 #29 实际为 **squash 式同步**（`c8c425163` 单提交，上游 SHA 不在 dev 线，merge-base 仍为 `0ab02020`）——§4b "merge 式定规" 与事实不符；本次 true merge 后 relay/user 相关文件与官方**逐字节一致**（`git diff official/main` 为空），假落后清零，此后上游同步必须走 true merge（`scripts/sync-upstream.sh`）
- **冲突 ×1**：`router/api-router.go` `GET /token` 上游新增 `UserCriticalRateLimit("access-token")` → 取官方语义
- **验证**：`go build ./...` 全绿；`go test ./router ./middleware ./common ./relay/...` 在 Linux（WSL）通过（Windows 本地 2 例 HTTP/2 GOAWAY 测试 `wsarecv: aborted` 为环境差异，CI=Linux 不受影响）；vision relay hook（controller/relay.go）与 `/api/relay-observer` 路由红线保留
- **UPSTREAM_BASE**：`0cd9dc85e` → `5c3abffe8`（v1.0.0-rc.24）

### 4d. 2026-08-09 合流记录（治理文档修复 + public 化 + CI 迁 hosted + audit 批次）

**治理文档批次（PR #61，audit-2026-08 #35/#46/#56）**：
- **UPSTREAM_BASE 语义定规（#46）**：从"上次合并的官方基线"改为"最近一次 sync 时的官方 HEAD 快照"；`scripts/sync-upstream.sh` merge 成功后自动 `git rev-parse official/main > UPSTREAM_BASE` 并入 merge 提交，并校验 `git merge-base HEAD official/main == official/main`（真落后 0），不一致输出告警。BASE 推进 `5c3abffe8` → `823e26304`（官方 #6674/#6711 尚未 merge 进 fork，落后 2，下次 sync 吸收；语义检测走 upstream-check.yml 的 `git cherry`，不因 BASE 跟随 HEAD 而失真）。
- **§2 对齐现实（#35）**：W0 seam 收缩方案改写为当前接缝事实描述（main.go 直连注册 + service 层 hook 封装 + setting 层配置注入），审计决定不实现 seam 收缩。
- **§3 幽灵条目清理 + owned dirs 界定（#56）**：删除 5 个无 fork 本地改动的条目（含 context_key/relay_info/billingexpr），补 owned dirs 作用域界定与当前 footprint 数字基线。

**仓库 public 化**：private → public（`gh api -X PATCH repos/.../new-api -f visibility=public`）。动机：消除 private 仓库的 Actions minutes/storage 计费（曾触发 account payments failed 锁定），public 仓库标准 hosted runner 免费不限分钟。同步清理 GHCR 历史版本释放存储（tokendance-komari/grok2api 整包删，mirai/fund-dashboard/diffaudit-*-runner/new-api 删旧版本保留 live）。

**CI 迁 hosted（PR #65）**：`ci.yml` 的 backend/frontend job 从 `[self-hosted, Linux, X64, wsl]` 改为 `ubuntu-latest`，加 `actions/setup-go@v6.5.0`（go-version-file: go.mod）+ `oven-sh/setup-bun@v2.2.0`（bun 1.3.11）。同 PR 含 rankings vendor fallback 修复（`model/model_vendor_fallback.go` + `service/rankings_vendor_fallback.go`，fork 独立文件，官方只 +2 行调用）。验证：Backend 2m25s + Frontend 42s 全绿。

**audit-2026-08 修复批次（4 PR）**：
- **#62 affinity-observer C 类**：affinity 软失败解绑（#39，连续 3 次 5xx 解绑不绑死 TTL）、磁盘体回读 1MiB 前缀（#43）、observer 丢弃告警（#52，连续 100/累计 1000 打 SysError）
- **#63 vision 5xx 日志**：5xx 失败路径补结构化日志（#47）+ 敏感 key 写时校验（#48）
- **#60 workflow trigger 修复 B 类**：ci.yml paths-ignore docs-only（#41）、docker-image-branch.yml cache scope+concurrency（#42）、release.yml/electron-build.yml tag 触发收紧（#37/#38）

**PR 收尾**：#30 关闭（内容已 upstream）、#34 转 draft（feat observability master/detail，VChart 在 happy-dom 无 canvas 需 mock，体量 993 行单独处理）；5 个 fix PR 合并后本地 6 worktree + 10 分支清理至 1 worktree + main/feat 分支。

**CI runner 注销待办**：`wsl-newapi` runner 不再被 ci.yml 引用但仍注册（#51 跟踪）；`sync-release-to-gitcode.yml` 用 self-hosted runner 且 gitcode 镜像对 public 仓库已无意义（#51 跟踪停用）。

## 1. 产品线清单（重放顺序即编号）

### P1 · Fork release / Docker / CI
- **自有目录**：`.github/`、`Dockerfile`、`VERSION`、`NOTICE`、`THIRD-PARTY-LICENSES.md`、`AGENTS.md`、`UPSTREAM_BASE`
- **上游覆盖**：ci.yml / docker-build.yml / release.yml（ci.yml 用 **hosted ubuntu-latest** + setup-go/setup-bun，2026-08-09 PR #65 迁移；release 构建用 hosted ubuntu-latest；GHCR 个人 owner；node 22 钉版；ci.yml 加 paths-ignore docs-only，2026-08-09 PR #60）；pr-check.yml 已删（2026-08-03，外部审核建议）
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
- **上游覆盖**（现状，即 §2 描述的接缝边界；2026-08-08 审计决定不做 seam 收缩）：
  - `main.go`（runtime 创建/Init/注入/Close）
  - `controller/relay.go`（attempt hooks 内联）
  - `service/quota.go` + `service/text_quota.go`（settlement 各 1 行 + TurnUsage 组装 5 行）
  - `router/api-router.go`（路由块内联）
  - `service/relay_observation.go`（请求路径 hook 封装：no-op 默认 + 事件组装/发布）
- **上游等价**：无（私有产品线）
- **重放**：见 §2 当前接缝；分 4 个 topic：core+storage → service 层 hook 封装 → Root API → frontend

### P5 · Dashboard / cache / keys UI
- **上游覆盖**：`web/src/features/dashboard/*`（3）、`web/src/features/keys/*`（3）、`web/src/features/system-settings/*`（4+models/ 卡片与 types 扩展）、`web/src/features/channels/*`（2）、`web/src/features/auth/*`（2）、`web/src/features/profile/*`（2）、`web/src/features/models/components/drawers/model-mutate-drawer.tsx`（ModelSettings 类型完整性）
- **冲突热点**：keys api/columns、system-settings/types 与官方 #6590 直接重叠
- **上游等价**：无

### P6 · 前端基建 overlay（extension 层）
- **上游覆盖**：`web/src/main.tsx`、`web/src/hooks/*`、`web/src/assets/brand-icons`、`web/src/routeTree.gen.ts`（生成文件，永不手工合并）、`web/package.json`、`web/bun.lock`
- **计划**（外部审核建议）：新增 `web/src/extensions/{navigation,sections,i18n}.ts` 编译期静态 overlay；上游文件只保留一次性 spread

### P7 · i18n
- **上游覆盖**：`web/src/i18n/locales/*`（en/zh/ja/fr/ru/vi/zh-TW 9 文件）
- **冲突热点**：6 个 locale 与官方重叠；上游每次整理 locale 都会撞
- **计划**（外部审核建议）：observer 翻译迁到 `web/src/features/observability/i18n/{en,zh}.json` + `addResourceBundle` 合入；其余语言 fallback 英文。此后自有 key 不再碰原生 locale 文件

### P8 · Vision Relay
- **自有目录**：`pkg/vision_relay/**`、`setting/model_setting/vision_relay.go`、`service/vision_relay.go`
- **上游覆盖**：`controller/relay.go`（单点钩子 4 行：预扣费后、retry 循环前调 `service.PrepareVisionRelayRequest`）——**修改预算硬规则：仅此一个上游文件**
- **依赖方向**：`controller → service/vision_relay.go（Gin/RelayInfo/BodyStorage 事务+错误映射）→ pkg/vision_relay（核心包无 NewAPI 运行时层依赖，仅依赖 common 基础 JSON wrapper、x/image、gjson/sjson；禁止依赖 controller/service/model/setting/relay/Gin/RelayInfo）`
- **禁止修改**（阶段 1）：`relay/**`、`relaykit/**`、`common/body_storage.go`、`constant/context_key.go`、`model/option.go`、`controller/option.go`、`main.go`、`web/**`
- **配置**：注册名 `vision_relay`（DB keys `vision_relay.*`，JSON 数组字段）；安全限制为包内常量（MaxImages=6/MaxDecodedBytes=15MB/MaxPixels=12M/并发 2/解码闸 2/调用闸 8）
- **状态**：**✅ 已完成并合入主线（2026-08-05，PR #12）**。T1–T6 全绿（T5 长稳 109/109 + T6 回归 8 项复验）、终审/团队审查修复完成（sidecall_secret 防泄露、A6 敏感词、出站 marker 接线、请求级熔断、严格解析）、设置 UI 已交付（模型设置页 Vision Relay 独立模块卡片）。**HK3 部署未执行**（用户指示不部署；前置阻塞仍为 Cerebras 29 渠道 auto-disable + gemma 上游不可用，见 `projects/gateway/STATE.md`）
- **上游等价**：核心可独立评估（核心包无 NewAPI 运行时层依赖——仅 common JSON wrapper，未来可上游化）

## 2. Relay Observability 当前接缝边界（现状）

> 2026-08-08 设计审计（audit-2026-08，#35）确认：W0 方案中的 `service/relay_lifecycle.go`、
> `service/relay_observer_bridge.go`、`router/relay_observer_routes.go` 从未落地，main.go 直接
> import 核心 overlay 包，原 §2 与代码脱节。审计决定**不实现 seam 收缩**，本节改写为当前实现的事实描述。

实际依赖方向（2026-08-09 实核）：

```text
main.go（直连注册：NewRuntime → Init → 双 SetRelayObserverRuntime → Close）
   │
   ├─▶ controller.SetRelayObserverRuntime / GetRelayObserver*（controller/relay_observer.go）
   ├─▶ service.SetRelayObserverRuntime（service/relay_observation.go，请求路径 hook 封装）
   │        controller/relay.go   → ObserveTurnAttemptBegin/End/Failure（attempt 循环 3 处）
   │        service/quota.go      → ObserveTurnSettlement（settlement 1 处）
   │        service/text_quota.go → ObserveTurnSettlement（settlement 1 处）
   ▼
pkg/relay_observer/               ← 核心（不 import gin/service/controller/model/setting）
```

当前接缝边界（事实）：
- **main.go 直连注册**：`var observerRuntime = relayobserver.NewRuntime()`（main.go:54），Init 后
  `controller.SetRelayObserverRuntime(observerRuntime)` + `service.SetRelayObserverRuntime(observerRuntime)`
  两行注入（main.go:73-75），退出时 `observerRuntime.Close(observerCtx)`（main.go:255）。main 直接
  import `pkg/relay_observer`——这就是当前的事实接缝（审计 #35 指出的脱节点）。
- **service 层 hook 封装**：`service/relay_observation.go` 承载全部请求路径 hook
  （AttemptBegin/End、Settlement、Failure）；运行时未接线时每个 hook 零开销 no-op，事件构造
  （buildTurnEvent）与发布（publishTurnEvent）都收在 service 层，controller 只调 `service.ObserveTurn*`。
- **controller 层接线**：`controller/relay_observer.go` 持 runtime + query surface（/status、/overview、
  /sessions… 的 GET handler）；`controller/relay.go` 内联 vision relay hook
  （`service.PrepareVisionRelayRequest` 单点 4 行，relay 失败 = 5xx，绝不 fail-open）。
- **router**：`router/api-router.go` 内联 `/relay-observer` 路由组（6 个 GET handler）。
- **setting 层配置注入**：`setting/model_setting/vision_relay.go` `config.Register("vision_relay", …)`
  → DB option keys `vision_relay.*`（enabled/target_models/models…）；`service/vision_relay.go` 做
  Gin/RelayInfo/BodyStorage 事务与错误映射，核心逻辑在 `pkg/vision_relay/`（无 NewAPI 运行时层依赖）。

硬规则（现行，与代码对齐）：
1. `pkg/relay_observer` 不 import gin / service / controller / model / setting。
2. `pkg/vision_relay` 不反向依赖 controller/service/model/setting/relay/Gin/RelayInfo（仅 common
   JSON wrapper、x/image、gjson/sjson）。
3. 删 observer = 删自有目录 + 移除接线点（main.go 2 行注入 + controller/relay.go 3 行 hook +
   service/quota.go & text_quota.go 各 1 行 + api-router.go 路由块），无隐式依赖残留。
4. PG 特例限制在 `pkg/relay_observer/store_pg.go` 与 migrations，不污染 `model/`。

> 演进备注：未来若做 seam 收缩（`service/relay_lifecycle.go` + bridge + 独立 routes），入口见
> audit-2026-08 #35，本节即为现状基线。

## 3. 冲突热点（B∩C，20 文件）

> 筛选口径（2026-08-09 起）：`git ls-files` 存在 + `git diff official/main..HEAD` 有本地改动。
> 审计 #56 清理 5 个幽灵条目（无 fork 本地改动）：`constant/context_key.go`、
> `relay/common/relay_info.go`、`service/tiered_settle.go`(+test)、`service/billing_session.go`、
> `pkg/billingexpr/*`（计费层已在上游演进中吸收，见 §1-P2 上游等价）。

按官方 #6590/#6589 引入时的预计冲突强度排序：

| 文件 | 本地改动性质 | 官方改动 | 冲突强度 |
|------|-------------|---------|---------|
| `controller/relay.go` | observer hooks 内联 3 处 + vision relay hook 4 行 | 未直接改（邻近） | 中 |
| `web/src/features/keys/*`（4） | 自有 UI | #6590 token 控制面 | 中高 |
| `web/src/features/system-settings/types.ts` | 自有扩展 | #6590 | 中 |
| `web/src/i18n/locales/*`（7 语言 + 5 同步报告） | 自有 key | #6590 新 key | 中（§1-P7 迁移后归零） |
| `router/api-router.go` | observer 路由块内联 | 未直接改 | 低 |
| `model/option.go` | 自有选项 | 未直接改 | 低 |

**owned dirs 界定**（§4 W3 "outside own dirs ≤8" 的作用域，footprint 审计基线）：
`pkg/vision_relay/`、`pkg/relay_observer/`、`service/relay_observation*`、
`web/src/features/observability/`、`web/src/i18n/`。当前 218 文件总 footprint 中 owned 110
（relay_observer 66 + vision_relay 11 + observability 18 + i18n 13 + relay_observation* 2），
outside-own-dirs 87（含 docs/.github/根治理 21）；重建目标 outside ≤8。

## 4. 影子重建流程（下一轮执行）

```bash
# W1 基线：从官方 HEAD 建重建分支（不 merge 当前 main）
git fetch official
git switch -c rebuild/upstream-20260803 official/main

# W2 按 §1 产品线顺序重放（P1 → P8），每线一个 topic commit：
#   P1 fork-release → P2 billing → P3 logging → P4 observer(4 topics) →
#   P5 dashboard/keys → P6 extensions → P7 i18n → P8 vision
# 重放前对每线 `git cherry` 去重，已被上游吸收的不重放

# W3 冲突面验收（owned dirs 界定见 §3）
git diff --name-only official/main..HEAD | grep -v '^\(pkg/relay_observer/\|pkg/vision_relay/\|service/relay_observation\|web/src/features/observability/\|web/src/i18n/\|controller/relay_observer\|web/src/extensions\|docs/\|\.github/\|AGENTS\|VERSION\|Dockerfile\|NOTICE\|UPSTREAM_BASE\)' \
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

## 5a. Release tag 命名规范与 GHCR 发版

### tag 命名

| 类别 | 格式 | 示例 | 说明 |
|------|------|------|------|
| fork 发版 | `v<major>.<minor>.<patch>-td-<YYYYMMDD>.<seq>` | `v1.0.0-td-20260809.1` | 跟随上游版本号 + td 标识 + 日期 + 同日序号；推送自动触发 docker-build.yml |
| 上游 rc 基线 | `v1.0.0-rc.<N>` | `v1.0.0-rc.24` | 上游 release tag，保持不动（追上游基线，UPSTREAM_BASE 指向对应 commit） |
| 临时同步 | `sync-<YYYYMMDD>-td<N>` | `sync-20260807-td10` | sync-upstream 产生的临时构建 tag，可清理 |

### GHCR 发版流程

1. main 上 audit/修复批次合并完成后，`git tag -a v1.0.0-td-<date>.<seq> -m "..."` 打 annotated tag
2. `git push public <tag>` → 自动触发 `docker-build.yml`（匹配 `v*-td-*`）
3. workflow 不可重建保护：同 tag 已发布则拒绝（immutability guard，digest 不变）
4. 镜像：`ghcr.io/deliciousbuding/new-api:<tag>` + `sha-<long>` 双 tag
5. 回滚锚保留策略：保留最近 2-3 个 fork 发版 tag + 1 个 sync 临时 tag；更老的清理

### GHCR 版本清理

- `gh api users/DeliciousBuding/packages/container/new-api/versions` 列版本
- 保留：最新发版 + 1-2 回滚锚；删过老的（`gh api -X DELETE .../versions/<id>`）
- 不可重建：已发版 tag 的镜像不可覆盖，需发新 tag 重建

## 5b. 分支命名规范与生命周期

### 主线

- **`main`**：唯一合流主线，受 ruleset（#20184444）保护（deletion + non_fast_forward + pull_request）。所有改动经 PR 合入，不直接 push。

### topic 分支命名（`type/topic[-YYYYMMDD]`）

| 前缀 | 用途 | 示例 | 日期后缀 |
|------|------|------|---------|
| `fix/` | bug 修复 | `fix/affinity-observer-20260808` | 带（便于追溯批次） |
| `feat/` | 新功能开发 | `feat/vision-relay-responses` | 不带（功能跨多日） |
| `docs/` | 文档变更 | `docs/fork-surface-baseline-20260809` | 带 |
| `chore/` | 杂项/清理 | `chore/cleanup-stale-handoff` | 不带 |
| `sync/` | 上游同步 | `sync/upstream-20260809` | 带 |
| `rebuild/` | 影子重建（一次性） | `rebuild/upstream-20260803` | 带 |

### 生命周期（2026-08-05 起强制）

- **功能收口 = 合入 main + 删分支**（本地 + 远端）。PR 合并时 `--delete-branch`。
- worktree 随分支删除而 prune（`git worktree prune`）。
- 不保留"在途"分支超过一个开发周期；长期搁置的 feat 转 draft PR 或关闭。
- 本地主 worktree 保持 `main`；topic 分支用独立 worktree（`.worktrees/<short-name>/`）。

### tag 与分支的关系

- 发版 tag（§5a `v*-td-*`）打在 `main` 的合并 commit 上，不从 topic 分支打。
- topic 分支不长期存活；合并后即删，tag 指向 main 的 commit 而非分支 HEAD。

## 6. 未决风险

- ~~`upstream-check.yml` 仍 `runs-on: ubuntu-latest`——private repo 无 hosted minutes，该检查实际不生效~~ → **已消除**（2026-08-09 仓库转 public，hosted runner 免费，该检查现已生效）。
- 上游周更频率高：建议 upstream status 日跑、实际 rebuild 周跑、大功能开发前手动跑。
- vision-relay（P8）与 observer 共用 context_key 文件，重放时注意追加式合并。
- UPSTREAM_BASE 落后 ~2 天（`5c3abffe8` vs 官方 `823e26304`），#46 跟踪 sync-upstream.sh 自动 bump；下次 sync 时 true merge 吸收。
