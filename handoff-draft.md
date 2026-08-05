# Relay Observability — Handoff（本地交接与生产提案边界）

最后更新：2026-08-02
基线：`codex/observer-p1-integration` HEAD=86edaec5f（含 sgp2 e2e 修复 17966aec0）

## 1. 产品线概览

原生 NewAPI Relay Observability：relay 请求的**旁路审计线**——turn（请求/响应元数据）、session（身份聚合）、content（预算内规范化内容）三级捕获，Root-only 查询 API + 原生前端 UI。**严格 fail-open**：审计线任何故障（DB 不可达/超时/panic）只禁用或降级审计，绝不改变 relay 响应、计费或 NewAPI 启动。开启条件：`RELAY_OBSERVER_ENABLED=true` + 有效 `RELAY_OBSERVER_SQL_DSN`（verify 模式要求 schema 已迁移；bootstrap 模式启动时自动迁移 v1→v3）。

## 2. 架构摘要

### 后端 `pkg/relay_observer/`（冻结契约，零 DB driver import 于契约层）

| 文件 | 职责 |
|---|---|
| `types.go` | 冻结契约：`Event`（turn 事件）、`Config`、`ContentState`（metadata_only/full/gap）、Store 端口 |
| `config.go` | `ConfigFromEnv()` 解析 `RELAY_OBSERVER_*`，数值 clamp 到 SSOT 硬上限；secret 字段全输出路径脱敏 |
| `identity.go` | 版本化 HMAC 身份别名（T2.1）：Codex 链 `turn_thread>turn_session>header_thread>header_session>cache_key`，Claude 链 `claude_session>meta_user_session>meta_session`；双 key 轮换 |
| `normalizer.go` / `codec.go` / `content.go` | 三格式归一化（openai chat / openai responses / anthropic messages）+ canonical codec + 预算截断 gap marker（marker 自身 digest，防碰撞） |
| `persistence.go` | `ContentPersistence` 端口 + append 编排（session 解析→去重→head 序列化→context 写入→计数） |
| `store_pg.go` | PG adapter：`WriteBatch`（turn 元数据，幂等 ON CONFLICT）、`AppendTurns`（content，同事务回填 turn.session_id）、retention 删除、schema 校验 |
| `dispatcher.go` | 队列 worker：`TryEnqueue`（字节预算 16MiB/事件 512 cap）、`planContent`（worker 侧归一化）、circuit breaker（指数退避）、pendingAppends 重试（cap 512） |
| `wiring.go` | `Runtime`/`ObserverRuntime` 接口、`QuerySurface()`（QueryStore+timeout）、HMAC 配置、测试缝 |
| `query.go` | keyset 分页查询：`ListSessions`/`ListTurns`/`Overview`/`GetSession`/`TurnContext`（reconstruct） |
| `settlement.go` / `attempts.go` | 计费结果捕获 / attempt 摘要（有界） |

### 迁移 `pkg/relay_observer/migrations/`
`001_v1.sql`（基础表+session 聚合+UNIQUE 幂等键）→ `002_v2.sql`（created_at 时间戳列+索引）→ `003_v3.sql`（keyset/model 复合索引 + verify 接受 [1,2,3]）。bootstrap 在短 advisory lock + 语句超时下建表。

### 请求路径 `service/relay_observation.go`
`buildTurnEvent`（纯内存同步构造，零 DB）→ `publishTurnEvent`（reservation=BodyStorage.Size()）→ dispatcher 异步写。`Identity` 材料在发布前快照（headers+body 引用）。

### 查询 API `controller/relay_observer_query.go`（Root-only，Router 层 403）
`GET /api/relay-observer/status|overview|sessions|sessions/:id|sessions/:id/turns|turns/:id/context`；degraded envelope（HTTP 200 + `data.degraded{reason:timeout|unavailable}`）；QueryError 分类（malformed_cursor/timeout/not_found → 400/503/404）。

### 前端 `web/src/features/observability/`
`api.ts`（fetch 封装）/ `types.ts`（zod snake_case DTO）/ `query-keys.ts`（react-query 键）/ `cursor-pagination.tsx`（显式 cursor 导航防爬页）/ `pages/overview-tab.tsx`、`sessions-tab.tsx`、`session-detail-tab.tsx`（URL-state seam）。Root 守卫 `beforeLoad` + `ROLE.SUPER_ADMIN`。i18n key=英文（en/zh 双语）。

## 3. 配置清单（`RELAY_OBSERVER_*`，config.go 权威）

| 变量 | 默认 | 硬上限 | 语义 |
|---|---|---|---|
| `RELAY_OBSERVER_ENABLED` | false | — | 总开关 |
| `RELAY_OBSERVER_SQL_DSN` | "" | — | PG DSN；空则禁用 |
| `RELAY_OBSERVER_SCHEMA_MODE` | verify | — | `verify`（版本不符即禁用）\| `bootstrap`（自动迁移） |
| `RELAY_OBSERVER_HMAC_KEY` | "" | — | 身份 HMAC 密钥（secret，不出现在 status/日志/API） |
| `RELAY_OBSERVER_HMAC_KEY_VERSION` | 1 | 2^30 | 当前代 |
| `RELAY_OBSERVER_PREVIOUS_HMAC_KEY` | "" | — | 轮换前一代 |
| `RELAY_OBSERVER_PREVIOUS_HMAC_KEY_VERSION` | 0 | 2^30 | 前一代版本 |
| `RELAY_OBSERVER_RECORD_IP` | false | — | 双 opt-in 之一：IP+GeoIP 捕获 |
| `RELAY_OBSERVER_QUEUE_SIZE` | 512 | 4096 | 队列事件上限 |
| `RELAY_OBSERVER_QUEUE_BYTES` | 16MiB | 64MiB | 队列保留字节预算 |
| `RELAY_OBSERVER_MAX_REQUEST_BYTES` | 8MiB | 16MiB | 单请求内容捕获上限 |
| `RELAY_OBSERVER_MAX_CAPTURE_BYTES_PER_TURN` | 8MiB | 16MiB | 单 turn canonical evidence 预算；与 queue reservation 解耦 |
| `RELAY_OBSERVER_BATCH_SIZE` | 32 | 128 | 写批大小 |
| `RELAY_OBSERVER_FLUSH_MS` | 1000 | 5000 | 刷盘间隔 |
| `RELAY_OBSERVER_WRITE_TIMEOUT_MS` | 2000 | 5000 | 单批写超时 |
| `RELAY_OBSERVER_QUERY_TIMEOUT_MS` | 500 | 2000 | Root 查询超时 |
| `RELAY_OBSERVER_RETENTION_TURN_DAYS` | 30 | — | turn 保留天数 |
| `RELAY_OBSERVER_RETENTION_CONTENT_DAYS` | 14 | — | content 保留天数 |

## 4. 构建 / 运行 / 验证

```bash
# 构建与单元测试（仓库根）
GOMAXPROCS=2 go build ./...
GOMAXPROCS=2 go test -p 1 ./pkg/relay_observer -count=1   # 80.4% 覆盖率，skipped=0
go vet ./pkg/relay_observer/  &&  gofmt -l pkg/relay_observer/  &&  git diff --check

# PG 集成测试（本机受限容器，禁碰外部 DB）
docker run -d --name tokendance-observer-pg-dev -e POSTGRES_PASSWORD=observer_test \
  -e POSTGRES_DB=relay_observer -e POSTGRES_USER=observer_test \
  -p 127.0.0.1:55433:5432 postgres:17.6-bookworm
export TEST_RELAY_OBSERVER_POSTGRES_DSN="postgres://observer_test:observer_test@127.0.0.1:55433/relay_observer?sslmode=disable"
GOMAXPROCS=2 go test -tags=relay_observer_pg_integration -count=1 ./pkg/relay_observer/

# 前端（web/）
cd web && bun test && bun run typecheck && bun run build

# 计费回归基线（T5 取证，GOMAXPROCS=2 -count=1 -benchtime=200x GOPROXY=off）
# BenchmarkTieredBilling_ComplexExpr 2394 ns/op 2160 B/op 44 allocs/op 等 4 项，见 T5.2 交付
```

## 5. 测试地图

| 文件 | 覆盖契约 |
|---|---|
| `contracts_test.go` / `config_test.go` | Store 端口/Config 解析 clamp 与脱敏 |
| `identity_test.go` | HMAC 身份链/轮换/预算降级 |
| `normalizer_test.go` / `codec_test.go` / `content_test.go` | 三格式归一化/golden 语料/HMAC 白名单/预算截断 gap |
| `dispatcher_test.go` / `content_wiring_test.go` | 队列预算/circuit/pendingAppends 重试/fail-open |
| `persistence_test.go` | append 编排（session 绑定/幂等/去重/rotation/冲突）+ **turn.session_id 回填** |
| `query_test.go` | keyset 分页/Overview/错误分类 |
| `store_pg_test.go` | PG adapter SQL 契约（幂等/列映射/脱敏） |
| `retention_test.go` / `retention_worker_test.go` | 6h 一次/每 pass 1000/100/1000 上限/孤儿清理 |
| `attempts_test.go` / `settlement_test.go` | attempt 摘要有界/计费捕获 |
| `*_pg_integration_test.go`（tags） | 真实 PG：全链 capture→reconstruct golden、locked 维度 degraded、retention 级联 |
| `benchmark_test.go` / `p99_delta_harness_test.go` | observer 基准 + relay 路径 enabled/disabled 对比（p99 delta 实测 268ns/1.7µs） |
| 前端 `__tests__/` | 236 测试全绿（T4.x 交付） |

## 6. 故障行为矩阵

| 故障 | 行为 | 证据 |
|---|---|---|
| 审计 DB 不可达/init 失败 | `store_init_failed` 确定性禁用，relay 零影响 | sgp2 实测：改错 DSN 重启 → status.Enabled=false，relay 200 |
| 写批超时 | 丢批 + circuit open（指数退避），请求路径无感 | dispatcher 测试 |
| 查询超时 | HTTP 200 + `data.degraded{reason:timeout}` | query/controller 测试 |
| 审计库挂起（advisory lock） | 短超时 → timeout 分类 → degraded | locked 集成测试 |
| content 捕获失败/panic | 该事件降级 metadata-only（计数 gap），不伤批/存储/请求 | content_wiring 测试 |
| 预算超限请求 | metadata-only turn（content 截断 gap marker） | sgp2 实测：body<marker 时纯 metadata |
| HMAC 未配置 | 身份解析失败 → 无 session 绑定，turn 正常记录 | identity 测试 |
| 迁移版本不符（verify） | 启动禁用，reason 明示 | config 测试 |
| 满队/字节预算耗尽 | 新事件 drop（计数），旧事件不受影响 | dispatcher 测试 |

## 7. 关键设计决策记录

- **两段式写入**：WriteBatch（turn 元数据）→ AppendTurns（content），分离故障域；pendingAppends cap=512 重试失败 content
- **turn.session_id 回填**（17966aec0，sgp2 e2e 发现）：content 解析出的 session 绑定同事务回写 turn 行——`sessions/:id/turns` 与 session EXISTS 过滤依赖它；此前恒 NULL 导致查询空页
- **身份 HMAC**：64-hex 全宽 digest，版本化双 key 轮换；gap marker 用自身 digest 防 (session,digest) 去重碰撞
- **幂等**：turn `ON CONFLICT (node_scope, event_id) DO NOTHING`；content `(session_id, item_digest)` UNIQUE
- **keyset cursor**：(occurred_at DESC, id DESC) / (last_seen DESC, id DESC) 复合索引；显式 cursor 防无限爬页
- **degraded envelope**：HTTP 200 + `data.degraded`（timeout/unavailable），非 5xx——客户端不重试
- **retention**：6h 固定 pass，≤1000 turn / 100 session / 1000 孤儿每 pass，session 级联删除

## 8. 生产提案边界（approvals）

**LOCAL_ONLY 期间禁止**：push/PR/GHCR 镜像/部署生产/hk3/Azure/生产 DB 写入/外发。当前集成状态：本地 worktree 分支 `codex/observer-p1-integration`（86edaec5f）+ sgp2 测试站（/opt/observer-test，已实测）。

**生产部署所需动作 + 批准**：
1. DB 迁移（生产 PG 执行 001→003）→ **需管理员批准**（生产 DB 写）
2. env 配置（RELAY_OBSERVER_* + HMAC key 生成）+ secrets 收口 → **需批准**（生产配置变更）
3. 镜像构建/推送 GHCR + 部署 → **需批准**（LOCAL_ONLY 解除）
4. 域名/网络暴露（查询 API 为 Root-only，若走公网需 CF + ACL）→ **需批准**（网络拓扑）
5. 上游 key（hk3 newapi-test）仅测试站用，生产用正式渠道 → 无需批准但需回收测试 key

**遗留（非阻塞）**：前端 UI 尚未在 sgp2 站人工走查（API 全通）；预算截断下小请求为 metadata-only turn 属设计行为。
