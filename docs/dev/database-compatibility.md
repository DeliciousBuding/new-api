# Database Compatibility — 三库方言实现细节

> 从 `AGENTS.md`「Database compatibility」下沉的实现细节。铁律条文留在 AGENTS.md（常驻），本文是按需层。

## relay_observer（PG-only 显式例外）

- `pkg/relay_observer/store_pg.go` 刻意 PG-only，包在小型 `Store` 接口之后；其余 NewAPI 保持库无关。
- 独立连接池（与 `model.DB`、`LOG_DB` 分离；max open 2 / max idle 1 / 60s lifetime）。
- versioned migrations（`pkg/relay_observer/migrations/*.sql`）为 PG 方言（`TIMESTAMPTZ`、`BYTEA`、advisory locks）。
- runtime 用 `pgx.ParseConfig` 拒绝非 PG observer DSN；失败时 observer 自禁用，绝不影响启动、relay 响应或计费。

## GORM 偏好

- 优先 `Create` / `Find` / `Where` / `Updates` 等方法，少用 raw SQL。
- 主键生成交给 GORM；禁止手写 `AUTO_INCREMENT` / `SERIAL`。
- `model/` 内 GORM 查询方法构造的标准 `SELECT ... FOR UPDATE` 行锁必须用 `lockForUpdate(tx)`：
  - 禁止 GORM v1 遗留写法 `tx.Set("gorm:query_option", "FOR UPDATE")`（GORM v2 静默忽略、不获锁）。
  - 禁止在调用点复制 `clause.Locking{Strength: "UPDATE"}`；共享 helper 对 MySQL/PostgreSQL 发 `FOR UPDATE`，对 SQLite（语法不支持）跳过。
  - 语义不同的方言锁（如 MySQL next-key/gap lock）仅允许在显式数据库类型分支后用 raw SQL，且每种库都有合法 fallback。

## Raw SQL 方言对照

- PostgreSQL 用 `"column"` 引号；MySQL/SQLite 用 `` `column` ``。
- 保留字列（`group`、`key`）用 `model/main.go` 的 `commonGroupCol` / `commonKeyCol`。
- 布尔值用 `commonTrueVal` / `commonFalseVal`。
- 主库分支 `common.UsingMainDatabase(...)`；日志库分支 `common.UsingLogDatabase(...)`。

## 跨库 fallback 与迁移

- 禁止无 fallback 的库专属特性：MySQL-only 函数、PostgreSQL-only 操作符、SQLite 不支持的 `ALTER COLUMN`、无 `TEXT` fallback 的库专属 JSON 列类型。
- 迁移必须三库可用；SQLite 用 `ALTER TABLE ... ADD COLUMN`（参考 `model/main.go` 模式）。

## GORM 布尔默认标签

默认值属于业务规则时避免 `gorm:"default:true"`——MySQL/PostgreSQL 对布尔默认的归一化不同，会导致 `AutoMigrate` 重启时反复 `ALTER TABLE`。优先在请求/模型归一化、hooks、构造函数或 service 逻辑里设默认；除非三库验证过，不要换成 `default:1`。
