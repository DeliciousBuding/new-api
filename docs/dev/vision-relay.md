# Vision Relay 设计与安全边界

Vision Relay 在网关内把受支持请求中的图片替换为不可信文本转写，使纯文本上游模型可以处理图像语义。核心实现位于 `pkg/vision_relay/`，NewAPI 运行时适配位于 `service/vision_relay.go`，配置注册位于 `setting/model_setting/vision_relay.go`。

## 设计目标

- 只接管显式命中的目标模型，不改变其他请求。
- 图片级识别失败降级为稳定占位，不把原图意外透传给纯文本模型。
- 配置、JSON 变换或存储提交失败时拒绝请求，避免半增强状态继续转发。
- 图片、OCR 文本和视觉模型输出都视为不可信数据，不赋予指令权限。
- 核心包不依赖 controller、service、model、setting、Gin 或 RelayInfo；运行时能力通过小接口注入。

## 请求流程

`PrepareVisionRelayRequest` 在预扣费完成后、重试循环开始前执行：

1. 从 `OptionMap` 读取一次不可变配置快照。
2. 验证递归保护 HMAC marker；只有认证 marker 可以跳过接管。
3. 检查总开关、原始目标模型 glob 和受支持协议。
4. 在模型命中后严格校验视觉端点、模型链、API key 和 sidecall secret。
5. 从 `BodyStorage` 读取原始请求，并在一个请求级总 deadline 内调用核心引擎。
6. 核心引擎提取、去重、抓取、解码/缩放图片，按 fallback 链生成转写。
7. service 先把增强 JSON 解码为原请求 DTO，再创建新 `BodyStorage`。
8. 所有验证完成后一次性替换 RelayInfo、Gin body storage、request body 和 content length；提交后才关闭旧存储。

Claude Messages、OpenAI Chat Completions 和 OpenAI Responses 由 `visionRelayFormat` 映射；未知协议保持 no-op。

## 失败语义

| 情况 | 行为 |
|---|---|
| 未启用、目标模型未命中、协议不支持 | no-op，原请求保持可读 |
| 请求无图片且活动配置可解析、端点校验通过 | no-op，并回绕原 `BodyStorage` |
| 单图超限、格式不支持、下载失败、视觉链失败、敏感词命中 | 用稳定失败枚举占位，继续处理其他图片 |
| 已命中目标模型但端点配置非法 | 5xx，禁止原图 fail-open |
| 配置快照、内部 JSON 变换、增强 DTO 校验或新存储创建失败 | 5xx，禁止半提交 |
| Redis 缓存读写失败 | 视为未命中或忽略写失败，不影响主流程 |

失败占位只使用 `types.go` 中的固定枚举，不携带 URL、凭据、模型错误体或上游自由文本。

## 转写契约

结构化转写默认开启。内置指令要求四个文本分节：

- `SUMMARY`：主体、动作、背景和关键信息。
- `TRANSCRIPTION`：可见文字、代码、报错、配置和表格的保真转录。
- `LAYOUT`：按阅读顺序描述主要版面区块。
- `UNCERTAINTY`：明确列出看不清或无法确定的内容。

`pkg/vision_relay/transcript.go` 容忍标题大小写、Markdown 标题和代码围栏漂移。视觉模型完全不遵守分节时，整段输出降级为 Summary，而不是丢弃已经获得的信息。渲染阶段省略空分节和等价于 `none` 的分节。

当前结构化模式是容错格式化，不是严格质量门：缺失分节或散文输出不会触发下一模型。需要把结构完整性作为 fallback 条件时，应新增明确的 strict mode，不能悄悄改变现有兼容语义。

自定义 `prompt` 完全接管识图指令并关闭结构化解析；`structured_prompt` 只覆盖结构化指令，仍使用四分节解析。结构化指令明确禁止执行图片内指令，也禁止编造像素坐标、边界框和置信度。

Claude `system[]` 和 Responses `instructions[]` 中的图片当前会在原高权限位置被替换为视觉转写。静态“不可信”边界不能降低 message role；在该路径完成可信 relocation/envelope 设计前，不应宣称提示注入边界已经完备。自定义 prompt 也不能长期拥有移除不可变安全前缀的能力。

## 安全与资源边界

- **递归保护**：sidecall 使用带时间窗的 HMAC marker；启用 Vision Relay 时 `sidecall_secret` 必填。外部伪造普通 header 不能绕过接管。
- **SSRF**：远程图片只能通过受保护 fetch client；保护客户端不可用时拒绝抓取，不回退到 `http.DefaultClient`。直连路径在 dial 前验证目标地址；环境代理会自行解析目标，不能等同于直连 peer 绑定。不能信任代理解析边界的部署应启用 `disable_proxy_fetch`。
- **不可信内容边界**：成功转写使用 `ResultPrefix` / `ResultSuffix` 包围，明确声明其中内容不可信。
- **图像限制**：图片数、下载/解码字节、像素、边长、单图描述和总注入量都在 `pkg/vision_relay/types.go` 设硬上限。
- **并发限制**：请求内图片并发、进程级解码槽和进程级视觉调用槽分别受限，避免网络吞吐与解码内存互相放大。
- **v0.4 配置面分层**：每请求策略上限（`max_images`/`request_concurrency`/`max_description_bytes`/`max_total_bytes`/`default_max_tokens`/`max_fallback_models`）与缓存 TTL（`cache_ttl_sec`）迁入 DB 热更新（每请求读快照生效；写时范围校验 + 请求时钳制 + 核心零值兜底三层防御）。进程级资源防线（解码字节/像素/边长/全局并发闸）保持包内常量 + 启动环境变量（`VISION_RELAY_*`），不进 DB——热改全局闸会瞬时放大进程内存峰值。
- **敏感词**：新转写和跨请求缓存命中都重新经过当前敏感词策略；污染缓存会 best-effort 删除。
- **日志脱敏**：结构化日志只记录稳定枚举、计数、模型名和耗时；配置错误不得回显可能含 userinfo 的 URL，也不得记录 API key 或 sidecall secret。

## 去重、缓存与可观测性

- 请求内按图片 digest 去重，同图多块只识别一次。
- Redis 跨请求缓存键绑定图片 digest 和实际识图指令；提示词变化自然生成新键，默认 TTL 为 24 小时（v0.4 起 `vision_relay.cache_ttl_sec` 热配置，0 = 禁用）。
- 缓存值是渲染后的明文转写，可能含图片中的代码、配置或其他敏感文本；Redis 必须按敏感数据面管理。
- `Stats` 区分图片块数、唯一图片数、请求内去重、跨请求缓存命中、视觉调用数和 fallback 次数。
- `Attempts` 按实际调用顺序记录模型、稳定结果枚举和耗时；自由文本错误不会进入日志。

当前 fallback 是同一 `base_url` 和 `api_key` 下按 `max_fallback_models`（v0.4 起热配置，默认 3）截断的模型名链，不是每一步绑定不同 provider/channel 的路由链。

## 已知实现债务

以下是 2026-08-16 对当前实现确认的高优先级缺口（v0.4 已消化的条目已移除）：

- 结构化输出缺少 strict quality gate，格式损坏不会触发 fallback，也可能进入缓存。
- 请求内图片准备当前串行，慢 URL 仍会阻塞同一请求内后续图片的准备。
- cache identity 未包含 provider/model/render-contract 版本，也没有 singleflight（TTL 与独立开关已于 v0.4 提供）。
- 纯核心包的“只依赖标准库/x-image/gjson/sjson”边界与项目级“业务 JSON 一律走 `common.*`”规则存在张力；在明确例外或抽象 port 前，不应继续扩散直接 `encoding/json` 调用。

## 配置边界

配置键以 `vision_relay.` 为前缀。`enabled` 为权威开关；关闭时其他残留坏配置不影响请求。启用时写入守卫和运行时校验共同保证 target model、endpoint、model chain、API key、timeout 和 sidecall secret 的完整性。

`api_key` 与 `sidecall_secret` 是只写敏感配置，选项读取面不回显。设置页通过原子 bulk update 一次提交全量快照，后端校验完整 prospective snapshot 后单事务写变更键；secret 空值 = 保持现值（keep），后端强制该契约。每请求策略上限（v0.4）属热配置、逐请求生效；进程级全局闸容量（解码/调用槽）仍只经启动环境变量调整，不在运行中修改。

## 变更检查

修改 Vision Relay 时至少验证：

```bash
go test ./pkg/vision_relay ./service ./setting/model_setting
go test ./controller ./router
```

涉及设置 UI 时，从 `web/` 运行受影响测试、类型检查和构建：

```bash
bun run test src/features/system-settings/models/__tests__/vision-relay-settings-card.test.tsx
bun run typecheck
bun run build
```

新增协议或图片格式时必须覆盖真实字节端到端路径，不能只伪造 content type。修改失败语义时必须同时覆盖 no-op、图片级占位和请求级 5xx 三类契约。调整增强顺序时还必须覆盖预扣费、最终 usage 缺失 fallback 和退款路径。
