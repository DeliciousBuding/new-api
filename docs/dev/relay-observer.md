# Relay Observer 架构与隔离契约

Relay Observer 是可选的原生 relay 可观测系统。它记录一次客户端请求对应的一次 turn、有限的渠道尝试摘要、会话身份别名和规范化对话内容，并提供 Root-only 查询界面。核心位于 `pkg/relay_observer/`，请求路径接入位于 `service/relay_observation.go`，HTTP 查询面位于 `controller/relay_observer*.go`。

## 不可破坏的隔离契约

- Observer 默认关闭；配置、连接、schema、写入、查询、retention 或关闭失败都不得改变 NewAPI 启动、relay 响应、重试、路由、日志写入或计费结果。
- `pkg/relay_observer` 通过小型端口隔离核心与存储；PostgreSQL driver、SQL 和迁移只属于 adapter。
- Observer 使用独立 PostgreSQL DSN 和独立小连接池，不复用主库或日志库连接。
- 请求路径只做有界内存快照和非阻塞 admission。未接线、禁用或关闭状态通过 lock-free 快路径保持 no-op。
- 队列同时限制事件数量和预留请求字节。设计要求超大请求降级为 metadata-only；当前实现只钳制 reservation，仍可能携带 request/body 引用，修复前不能把队列字节数视为真实内存上界。
- worker 批量写入，失败批次不阻塞请求，也不在请求路径重试；连续失败由写入 circuit 控制。
- 请求路径入口吸收 panic；状态和 API 只暴露稳定 reason code。部分 worker/query 日志当前仍输出原始错误文本，必须按稳定分类收口后才能满足完整日志边界。

`pkg/relay_observer` 是根模块的 fork-only 子系统，不是 `relaykit/` 的一部分。它可以使用根模块的公共工具，但不得反向让 `relaykit/` 依赖 Observer。

## PostgreSQL-only 例外

主 NewAPI 数据访问仍须同时支持 SQLite、MySQL 和 PostgreSQL；只有 Relay Observer 被明确允许使用 PostgreSQL 方言。`OpenPGStore` 解析并拒绝非 PostgreSQL DSN，失败后 runtime 自禁用。

结构迁移 v1-v4 位于 `pkg/relay_observer/migrations/`，并发索引迁移 v5-v8 由 `pkg/relay_observer/store_pg.go` 管理：

- migration 版本记录必须与实际对象一致，重复启动保持幂等。
- 普通 schema bootstrap 在短 advisory lock、lock timeout 和 statement timeout 内执行。
- 大表索引通过并发索引路径创建，避免阻塞 relay 写入。
- 查询计划所依赖的 session、turn、retention 和 alias 索引属于 API 性能契约，不能当作内部细节随意删除。

数据库方言例外和连接池边界另见 `docs/dev/database-compatibility.md`。

## 数据流水线

### Turn 与尝试

一次客户端请求最多发布一个最终 turn：成功路径在 settlement 和 consume log 完成后发布，失败路径在重试耗尽后发布。尝试摘要只包含 channel id、group、status/error code 和耗时，不记录上游自由文本错误。

尝试数量有硬上限；保留策略是前部尝试加最终尝试，并单独记录 omitted 数量。这样既能解释 fallback，又不会让异常重试链无限增大事件。

### 会话身份

worker 从受支持协议的稳定字段提取别名，按 provider/scope 和版本化 HMAC 生成不可逆 digest。原始 token、credential、prompt cache key 或 session id 不落库。HMAC 轮换同时支持 current/previous key：旧别名命中后采用既有 session，再绑定新版本 digest。

会话归属由 alias scope 决定，不由可伪造的 `client_profile` 决定。相同底层 Claude Code 会话即使从 CLI、VS Code 或第三方桌面壳访问，也不会因为展示 profile 不同而被拆成多个会话。

### 内容规范化与重建

请求 DTO 在 request path 最后一次写入后以共享只读引用进入队列；channel send 建立 happens-before，worker 才开始读取。磁盘型 `BodyStorage` 不整包读回内存，无法从 body 获得的身份信息按缺失处理。

worker 把 Claude、OpenAI Chat 和 Responses 请求归一成有序 canonical items。每项内容受大小和数量限制，媒体只保留受控元数据，不保存原始图片或音频 payload。截断必须写显式 gap marker，不能静默丢失。

内容对象按 session + digest 去重并使用 zstd 存储。上下文采用一条 full checkpoint 加最多八条 delta 的固定组：重建一条 delta 只读取一条 full 和一条 suffix，禁止递归 delta 链。写入只锁当前 session head，不持有表级锁。

## 查询与权限边界

所有 API 位于 `/api/relay-observer` 并受 `middleware.RootAuth()` 保护：

- `GET /status`：只读进程内状态，不访问数据库。
- `GET /overview`：固定窗口聚合和总量。
- `GET /sessions`、`GET /sessions/:id`：会话列表和详情。
- `GET /sessions/:id/turns`：turn 元数据页。
- `GET /sessions/:id/transcript`：有界扁平对话页。
- `GET /turns/:id/context?session_id=...`：单 turn 上下文重建。

列表使用 keyset cursor，不使用 offset；page size、窗口数、transcript 行数和返回内容都有硬上限。进程内只允许一个数据库型 Root 查询并发执行，context timeout 与 SQL `LIMIT` 是独立后盾。列表查询不能读取 content payload；只有 context/transcript 路径按重建边界读取内容对象。

store 失败和 timeout 使用 HTTP 200 degraded envelope 表达可观测系统降级，不能扩大成主业务故障。非法参数/游标使用稳定 400 code，身份不存在使用稳定 404 code；内部错误文本不进入响应。

## 隐私与基数治理

- status、reason code、日志和 API 不得包含 DSN、HMAC key、原始 alias 或 credential。
- IP/GeoIP 是双重 opt-in：Observer 配置允许记录且全局 IP 日志开关开启时才捕获；每条 turn 同时记录 `direct`、`proxy` 或 `none` 信任层级。
- `client_profile` 和 User-Agent 都是客户端可伪造的展示 hint，不能用于认证、授权、计费或路由。
- error type/code 必须来自稳定分类，不存原始上游错误文本。
- 新增筛选维度前必须评估可伪造性、隐私、索引成本和 cardinality；不能把任意 header/value 直接变成数据库维度。
- retention 分别控制 turn/session 和 content 生命周期；删除前在事务内重新检查 session recency，避免 list/delete 窗口误删活跃会话。

规范化文本、tool 参数和 tool 输出使用 zstd 压缩，但没有加密；数据库操作者仍可读取。`retention_content_days` 当前是孤儿对象 grace period，不是已引用内容的硬 TTL。HMAC 内容验证只支持 current/previous 两代 key，轮换周期必须覆盖有效内容保留期，直到引入带版本 key ring。

## 已知实现债务

以下是 2026-08-16 对当前实现确认的高优先级缺口，不属于已兑现能力：

- GeoIP enrichment 尚未接入生产事件，因此 Observer 只提供受信任等级和原始 IP（在双重 opt-in 开启时）；country/ASN 不属于当前查询或 UI 过滤维度。

## 配置所有权

启动时固定配置来自 `RELAY_OBSERVER_*` 环境变量：enabled、独立 DSN、schema mode、HMAC key/current version、previous key/version、IP opt-in、队列/捕获限制、batch/flush/write/query/retention budget 和默认保留期。解析失败会禁用 Observer，不会阻止进程启动。

只有以下运行参数通过 options 热更新，并在每个使用点重新读取：

- `relay_observer.query_timeout_ms`
- `relay_observer.retention_turn_days`
- `relay_observer.retention_content_days`

DSN、schema mode、HMAC keys、enabled 和队列结构属于启动配置，不能伪装成热配置。完整默认值和硬上限以 `pkg/relay_observer/config.go` 为实现源。

## 验证矩阵

纯逻辑和故障隔离：

```bash
go test ./pkg/relay_observer ./service ./controller ./router
```

PostgreSQL adapter、迁移、锁和查询计划必须再运行仓库现有的 `relay_observer_pg_integration` 测试入口。测试拒绝非本机 disposable PostgreSQL：只接受 loopback、端口 `55433`、数据库 `relay_observer`。没有 PostgreSQL 证据时，不能声称 migration/index/locking 改动已验证。

```bash
TEST_RELAY_OBSERVER_POSTGRES_DSN='postgres://...@127.0.0.1:55433/relay_observer' \
  go test -tags relay_observer_pg_integration ./pkg/relay_observer
```

涉及前端 Observer 页面时，从 `web/` 运行：

```bash
bun run test src/features/observability
bun run typecheck
bun run build
```

任何 request-path 改动都必须证明 Observer 关闭、store 锁死、查询锁死、writer 失败和 retention 失败时主 relay 仍保持原响应与计费语义。修复 schema/index 时还必须把 tagged PostgreSQL migration lifecycle 更新到当前版本；默认单元测试不能替代真实 PG 证据。
