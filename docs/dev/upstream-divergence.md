# 上游分歧台账（Upstream Divergence Ledger）

> 维护对象：本 fork 对上游 `QuantumNous/new-api` 主仓库**已有文件**的改动。
> 用途：每次把上游 `main` 合入 `dev`（`git merge official/main`）解冲突时，对照本表判断「这个冲突该保留 fork 侧还是上游侧」。
> 注意：本表只记录**类型一（fork 拥有的分歧）**；**类型二（同步滞后）** 会自动消除，不记录，见下文。

## 背景与两类 diff

`git diff official/main dev` 看到的不同，有两种来源，必须分开对待：

| 类型 | 含义 | 处理 |
|---|---|---|
| **类型一：fork 拥有** | fork 提交改动了上游已有文件 | 需长期维护，本表记录 |
| **类型二：同步滞后** | `official/main` 走在前、`dev` 尚未 merge | merge 时自动消除，**不是我们的负担，不记录** |

用下面命令区分两类（`<file>` 上若有 fork 提交，即类型一；为空即类型二）：

```bash
# 某文件是否被 fork 提交动过（>0 = 类型一，=0 = 类型二）
git log official/main..dev --oneline -- <file> | wc -l

# 当前所有类型一文件（fork 拥有的上游 Go 文件，按改动次数降序）
git diff official/main dev --name-status | awk '/^M/ && $2 ~ /\.go$/ {print $2}' | \
  while read -r f; do
    n=$(git log official/main..dev --oneline -- "$f" | wc -l)
    [ "$n" -gt 0 ] && printf "%s\t%s\n" "$n" "$f"
  done | sort -rn
```

## 风险分级

- **高**：上游也在高频改动此文件（自 2026-06 起 ≥10 次上游提交），merge 冲突概率大，改动需特别克制。
- **中**：上游偶发改动，或改动点集中、易对齐。
- **低**：测试/纯增量点，基本不冲突。

## 台账

### 客户端画像 / 日志

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `service/log_info_generate.go` | UA/客户端画像检测：Codex VS Code 发起方分类、第三方 Claude Desktop 识别、Claude 变体画像、品牌图标 | `508d145c6` `abadd4f99` `738d6d98e` | `docs/dev/client-profile.md` | 中 |

### 路由 / 设置

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `router/api-router.go` | vision/settings 原子更新路由 + observer transcript API 路由 + cache 用量预聚合状态路由 | `787da7e5a` `7c0205bc4` | `docs/dev/vision-relay.md`、`docs/dev/relay-observer.md`、`docs/dev/database-compatibility.md`（Cache 用量聚合表节） | **高** |
| `controller/option.go` | 设置端点：批量原子更新、完整写入守卫、secret keep/clear 契约 + cache_usage_aggregation 写侧校验 case | `787da7e5a` | `docs/dev/vision-relay.md` | 中 |

### 计费 / 配额（billing）

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `service/token_counter.go` | vision 增强 prompt token 计费 | `a03189eb8` | `docs/dev/vision-relay.md`、`docs/dev/billing-safety.md` | 中 |

### relay observer 观测点

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `service/quota.go` | observer 结算钩子（`ObserveTurnSettlement`） | `847829bca` | `docs/dev/relay-observer.md` | 中 |
| `service/text_quota.go` | observer 结算钩子（`ObserveTurnSettlement`） | `847829bca` | `docs/dev/relay-observer.md` | **高** |

### Vision relay 安全（SSRF）

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `service/http_client.go` | vision relay SSRF fail-closed + 无条件 sidecall_secret | `77cb2efa5` | `docs/dev/vision-relay.md` | 中 |
| `service/protected_fetch_client.go` | SSRF-safe relay fetch 专用 client | `1e96966b7` | `docs/dev/vision-relay.md` | 低 |

### 渠道亲和（channel affinity）

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `service/channel_affinity.go` | issue #39 软失败解绑 | `847829bca` | —（fork-owned 逻辑可下沉） | 中 |
| `service/channel_affinity_template_test.go` | 同上（测试） | `847829bca` | — | 低 |
| `service/channel_affinity_usage_cache_test.go` | 同上（测试） | `847829bca` | — | 低 |

### 排序 / vendor fallback

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `service/rankings.go` | 确定性 vendor 前缀匹配（配套 fork-owned `rankings_vendor_fallback.go`） | `6b6a089c3` | `service/rankings_vendor_fallback.go` | 中 |

### relay 协议转换（relaykit）

> 曾改动 `relaykit/relayconvert/internal/oai_chat/to_claude_messages_req.go`（不注入空 tools，`04fc5c49b`）；上游 #6862 已合入等价修复，现该文件与 upstream **零差异**，不再计入台账。relaykit 其余文件与 upstream 一致。

### 上游错误诊断（SSE，事故修复 #143 + 加固 #152）

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `service/error.go` | `RelayErrorHandler` 钩子：JSON 解析失败时提取 SSE 错误诊断（不改客户端语义）；#152 换用 `FormatUpstreamErrorDetail` 格式化并清洗换行 | `ac6944e27` `8fdbbff9e` | — | 中 |
| `service/error_test.go` | RelayErrorHandler SSE 诊断契约测试 | `ac6944e27` | — | 低 |
| `relaykit/types/error.go` | 新增 `UpstreamErrorDetail` 结构化诊断类型（#152 删 `PayloadFormat`/`String()`，transport/日志关切归 host） | `ac6944e27` `8fdbbff9e` | `relaykit/README.md` | 中 |

> 提取器本体是 fork-owned 新文件 `service/upstream_error_extract.go`（上游不存在，永不冲突），不在本表。

### Relay 韧性（尝试预算 + 状态归因，#155）

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `service/channel_select.go` | `RetryParam.attempts` 单调尝试计数 + `UpstreamBudgetExhausted`（不随 auto-group 切换重置，封顶单请求上游尝试数） | #155 | — | 中 |
| `common/constants.go` | `MaxUpstreamAttempts` 全局上限（默认 0=无上限，保持历史行为） | #155 | — | 低 |
| `model/option.go` | `MaxUpstreamAttempts` option 接入 | #155 | — | 低 |
| `controller/relay.go` | 每次真实上游尝试 `IncreaseAttempts`；`shouldRetry` 后查预算上限 | #155 | — | 中 |
| `relaykit/types/error.go` | `upstreamStatusCode` 字段 + Get/Set（记录 status_code_mapping 改写前的真实上游码） | #155 | `relaykit/README.md` | 中 |
| `service/error.go` | 映射改写时 `SetUpstreamStatusCode` | #155 | — | 低 |

> 与上游 #6580（exclude-driven failover）机制不同、不重复：本处是**尝试预算上限 + 映射前状态归因**，#6580 合入后可共存。`MaxUpstreamAttempts` 默认 0=不改变行为，运维显式配置才生效。

### 结构化错误码分类 + 审计（#156 #157）

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `service/channel.go` | `ShouldDisableChannel` 按结构化码 `IsAccountFatalError` 直接禁用（不依赖关键词，SSE 体解析失败时仍可靠） | #156 | — | 中 |
| `controller/relay.go` | `shouldRetry` 按 `IsNonRetryableUpstreamError`（账户/鉴权级）快速失败 | #156 | — | 中 |
| `controller/channel.go` | `auto_ban` 变更进 `changed_fields` + 记录前后值（1→0 漂移归因） | #157 | — | 中 |

> 分类器本体是 fork-owned 新文件 `service/upstream_error_classify.go`（保守精确匹配，未知码原样走既有状态码/关键词逻辑），不在本表。

### 渠道测试（channel test）

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `controller/channel-test.go` | 渠道测试 body 错误探测复用统一 SSE 提取器（#143）；#152 改为「保留上游 gjson 检测器 + 追加提取器兜底」，不再有损替换 | `ac6944e27` `8fdbbff9e` | — | **高** |
| `controller/channel_test_internal_test.go` | SSE 探测测试（#143） | `ac6944e27` | — | 中 |
| `relay/channel/api_request_test.go` | header override 锁定进渠道校验（测试） | `1f0aca0a1` | — | 低 |

### 基础设施 / 基线（fork 初始化时一次改动）

| 文件 | 改动理由 | 代表提交 | 治理文档 | 风险 |
|---|---|---|---|---|
| `main.go` | observer runtime init（fail-open）+ geoip 引入 + embed 基线 | `847829bca` | `docs/dev/relay-observer.md` | **高** |
| `controller/relay.go` | vision relay 钩子 + observer 观测 + channel affinity 软失败解绑 + SSE 诊断落库（#152：message 脱敏 + request_id 截断 + 每轮重试清 key） | `847829bca` `8fdbbff9e` | `docs/dev/vision-relay.md`、`docs/dev/relay-observer.md` | 中 |
| `controller/log.go` | 缓存用量聚合统计 + 90 天窗口限制 + cache 用量预聚合三段选路（水位 fail-safe 回退全实时） | `847829bca` | `docs/dev/database-compatibility.md`（Cache 用量聚合表节） | 中 |
| `model/log.go` | 日志 locality 提示（`attachGeoInfoToOther`/geoip）+ 品牌注释 scrub | `847829bca` `c178e645e` | — | **高** |
| `model/option.go` | `LogRecordIpEnabled` 选项接入 | `847829bca` | — | 中 |
| `middleware/distributor.go` | channel affinity 软失败计数重置 | `847829bca` | — | 中 |
| `model/main.go` | cache 用量预聚合两表 AutoMigrate（token_cache_usage_hourly / cache_usage_aggregation_meta） | — | `docs/dev/database-compatibility.md`（Cache 用量聚合表节） | 低 |
| `model/system_task.go` | cache 用量聚合任务类型常量 | — | `docs/dev/database-compatibility.md`（Cache 用量聚合表节） | 低 |
| `controller/system_task_handlers.go` | 注册 cache 用量聚合定时任务（一行；handler 定义在 fork 文件 `controller/cache_usage_aggregation.go`） | — | `docs/dev/database-compatibility.md`（Cache 用量聚合表节） | 低 |
| `common/constants.go` | `LogRecordIpEnabled` 开关 + 品牌注释 scrub | `847829bca` `c178e645e` | — | 中 |
| `logger/logger.go` | 日志计数/状态原子化（`atomic.Int64`/`atomic.Bool`，并发安全） | `847829bca` | — | 低 |
| `model/log_format_test.go` | 日志格式化测试基线 | `847829bca` | — | 低 |
| `service/task_polling_test.go` | task 轮询测试基线 | `847829bca` | — | 低 |

## fork-owned 目录（永不与上游冲突）

以下目录/文件上游不存在，纯增量，`git merge official/main` 不会产生冲突，故不在台账内：

- `pkg/relay_observer/`、`pkg/vision_relay/`
- `model/model_vendor_fallback.go`、`service/rankings_vendor_fallback.go`
- `docs/dev/*`（本文件所在目录，上游不存在）
- 前端 fork 自有 feature 文件（`web/src/features/**` 中上游没有的部分）

> `relaykit/` 上游存在同名独立 go module（上游 #6369 抽出），不算 fork-owned；当前 fork 对其 **零差异**（曾有一处改动已被上游 #6862 吸收），见上表 relaykit 条目。

## 上游收敛计划（过渡方案 → 上游合入 → 退役/吸收）

> 以下 fork 实现是**过渡方案**：上游正在同一问题域推进（流错误分类/日志），其 PR 合入后应评估收敛，避免两套并存。**每次 sync 上游后对照此节复查。**

### 过渡方案清单

| fork 实现 | 位置 | 上游对应物（2026-08-20 均 open） | 收敛动作 |
|---|---|---|---|
| SSE 错误体提取器（#143/#152） | `service/upstream_error_extract.go`（fork-owned） | #6523 `isOpenAITextStreamErrorChunk`、#6446 流内 429 重试 | #6523 合入后：评估直接用上游检测结果喂 `admin_info.upstream_error`，退役 fork 提取器 |
| channel-test 追加探测 | `controller/channel-test.go`（上游文件内 additive 6 行） | 上游 #6917 gjson 检测器（已合，我们保留原样） | 冲突时优先保留上游；fork 探测保持 additive 不变 |
| `UpstreamErrorDetail` 类型 | `relaykit/types/error.go` | —（上游暂无同概念） | 上游若在 relaykit 引入同概念：fork 侧回退 `Metadata`；host 格式化/transport 关切**永不回迁 relaykit**（#152 已删 PayloadFormat/String） |
| 异常流中断追踪缺口 | 无（现仅靠 `stream_status.end_reason`，如 `client_gone`） | #6927（type=5 `stream_incomplete` transport 错误日志） | #6927 合入后吸收：核对它的字段（`error_type=transport` 等）与 `admin_info` 不打架，补上 `client_gone` 场景的独立 transport 错误记录 |

### 上游同域追踪

| 上游 | 状态 | fork 侧影响 |
|---|---|---|
| #5222 流错误分类 issue | open | 上游总纲，与 #152 方向一致 |
| #6446 pre-stream 429 重试 | open | 与 #143/#152 相邻，无冲突 |
| #6523 流错误检测 + 渠道回退 | open | **收敛主目标**（见上表第一行） |
| #6580 exclude-driven failover | open | 我们的 #145 直接采用，勿自造 |
| #6927 异常流中断日志 | open | 补 `stream_incomplete` transport 错误，吸收时核对字段 |
| #6938 xAI 流内错误转发 | open | xAI 专用，与我们的非 2xx SSE 提取**互补**（不同机制） |

### sync 后复查（体检命令）

```bash
# 1. 分歧体检：fork 改动的上游 .go 文件（每个都应能在本台账找到理由）
git diff official/main dev --name-status | awk '/^M/ && $2 ~ /\.go$/ {print $2}' | \
  while read -r f; do n=$(git log official/main..dev --oneline -- "$f" | wc -l | tr -d ' '); \
  [ "$n" -gt 0 ] && printf "%s\t%s\n" "$n" "$f"; done | sort -rn

# 2. relaykit 独立可构建（host 依赖漏入 = 设计 bug）
cd relaykit && GOWORK=off go build ./... && GOWORK=off go test ./types/ -count=1

# 3. 上游落后度（应为 0；>0 立即评估 merge）
git rev-list --count dev..official/main
```

## 维护流程

1. **改上游文件前**：先确认能否用 fork-owned 文件实现（`docs/dev/`、`pkg/*/`、`model/*_fallback.go`、`service/*_fallback.go`）。能就不用改上游文件。
2. **必须改上游文件时**：只打最小钩子，逻辑放 fork-owned 文件；改完更新本表对应行。
3. **同步上游时**：`main` 由 `upstream-sync.yml` 每日强制同步，勿手改；把 `official/main` 合入 `dev` 时，冲突文件对照本表判断保留 fork 侧还是上游侧。
4. **定期体检**：`dev` 落后 `official/main` 越多，merge 冲突越大；保持低频次、小步 merge（当前落后 0 个提交，已吸收至上游最新，2026-08-19）。
5. **热点文件红线**：`router/api-router.go`、`model/log.go`、`main.go`、`controller/channel-test.go`、`service/text_quota.go` 为上游高频改动文件，非必要不新增改动，必要改动前在对应 issue 里声明理由。
