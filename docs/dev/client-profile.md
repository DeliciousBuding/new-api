# Client Profile 分类契约

Client Profile 是从请求 header 和 User-Agent 推断出的细粒度客户端展示标签。后端入口是 `service.DetectClientProfile`；结果写入管理员可见的 usage log，并随 Relay Observer turn 持久化。它只用于排障和趋势观察，不是可信身份。

## 信任边界

- header 和 User-Agent 都由客户端控制，任何 profile 都可被伪造。
- profile 不得参与认证、授权、计费、渠道路由、限流豁免或安全策略。
- 未识别请求使用通用 fallback；不得为提高命中率而把普通浏览器或过短通用词映射到品牌。
- Observer 只保存受控低基数 profile。usage log 另外把原始 User-Agent 截断到 256 字节后放在 `other.admin_info.client_ua`，仅供管理员核对分类依据；它不能成为 Observer 聚合、筛选或索引维度。

## 识别优先级

分类按信号特异性排序：

1. 官方或生态特征 header，例如 Codex `Originator` / `X-Codex-*`、Claude `X-App`、LiteLLM header。
2. 高特异性 User-Agent 词，先匹配具体 variant，再匹配家族 fallback。
3. 协议生成器 header，例如 Anthropic version 或 Stainless 信息，只在没有品牌信号时识别 SDK。
4. 通用 HTTP 客户端和 `chat` fallback。

顺序是契约的一部分。新规则必须证明不会被已有短词吞掉，也不会抢占更具体的 variant。例如 `opencode` 必须先于泛化 Codex 匹配，Claude VS Code/第三方桌面 variant 必须先于 `claude-cli` fallback。

## Observer 中的语义

每个 turn 保存当次细粒度 `ClientProfile`。session 的 `client_family` 是首次观察到的展示 profile，并保持 first-seen sticky；后续 turn 不覆盖它。

会话分组不能依赖 profile。身份 alias 使用协议稳定字段、scope 和版本化 HMAC；因此同一底层会话从 CLI、VS Code 或桌面壳访问时仍可归入同一 session。修改 profile taxonomy 不得改变 alias digest、session claim 或 rotation adoption。

## Taxonomy 变更规则

新增、改名或合并 profile 必须同步完整消费面：

1. 在 `service/log_info_generate.go` 添加最小、可解释的检测规则。
2. 在 `service/log_info_generate_test.go` 用真实 header/UA fixture 覆盖命中和相邻误判。
3. 更新 `web/src/features/usage-logs/types.ts` 的 `ClientProfile` union。
4. 更新 `web/src/features/usage-logs/lib/format.ts` 的展示标签。
5. 在 `client-profile-badge.tsx` 选择官方品牌图标或中性 fallback，并覆盖 badge 测试。
6. 检查 Observer session/turn 页面能处理新值；需要新增用户文案时同步全部 locale。

不要为单个样本添加宽泛子串。新增品牌规则应至少有官方源码、公开文档或可复现的实际样本来源；来源只需在邻近代码注释中简述，不在规则入口堆积考古叙事。

## 基数与兼容性

- profile 值是封闭、低基数的 snake_case 枚举；禁止版本号、平台版本、用户标识或任意 header 值进入标签。
- 原始 `client_ua` 只属于管理员 usage-log 诊断证据，必须保持长度上限和 UTF-8 边界，也不能提升为普通用户可见字段。
- 已落库值可能长期存在。前端必须对未知历史值安全降级，后端不能依赖前端 union 做数据完整性保证。
- 改名会把同一客户端拆成新旧两段历史数据。只有语义确实错误时才改名；展示文案变化优先保持存储值稳定。
- `client_family` 用于 Observer 过滤和聚合，新增高基数或短生命周期 variant 前必须确认运营价值大于索引与查询碎片成本。

## 验证

后端分类与 Observer 展示契约：

```bash
go test ./service ./pkg/relay_observer ./controller
```

前端从 `web/` 运行：

```bash
bun run test src/features/usage-logs/components/__tests__/client-profile-badge.test.tsx
bun run test src/features/observability
bun run typecheck
bun run build
```

评审时应检查：具体规则顺序、假阳性、未知值降级、session grouping 不变，以及 profile 未流入任何权限或计费分支。
