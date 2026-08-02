# Vision Relay — 网关层原生图片识图替换 设计文档（v0.1 供审核）

最后更新：2026-08-03

> 本设计文档基于两个 subagent 的可行性研究（只读，未改代码），代码级事实均已
> 对照本仓库当前源码核实（文件路径:行号为实测值）。**待 GPT 审核**：重点审核
> §5 可能的问题、§6 需要深入研究设计的点、§7 开放决策。

## 1. 背景与目标

**现状**：客户端侧 vision-bridge hook（Claude Code PreToolUse 全工具锚点）已实现：
纯文本模型（deepseek 系）收到贴图时，hook 从 transcript 提取图片 base64 → 调
tokendancelab 网关视觉模型（gemma-4-31b → step-3.7-flash → grok-4.5 fallback 链）
识图 → 文字描述注入上下文。已稳定运行，但依赖客户端 hook。

**目标**：把该能力**下沉到网关层原生实现**——本网关（NewAPI fork）收到带图请求时
自动拦截 image 块，调外部视觉模型生成文字描述，替换为 text 块后转发上游。效果：
- 任意客户端（Claude Code / OpenAI 兼容客户端）发带图请求 → 纯文本上游模型直接"看到"图
- 覆盖客户端 hook 够不到的场景：FileRead 工具读图的 tool_result 内嵌块、computer use 截图
- 客户端完全无感（无请求体签名校验，已核实），无需任何客户端改造

**约束（已核实的事实基础）**：
- 本仓库是 QuantumNous/new-api fork（go.mod:1 `module github.com/QuantumNous/new-api`，
  go 1.25.1，VERSION `v1.0.0-main-td-20260801.14`）
- 客户端（claude-code）image 块形态：`{"type":"image","source":{"type":"base64","media_type","data"}}`，
  纯 base64 无 data URL 前缀；media_type 白名单 png/jpeg/gif/webp；base64 ≤5MB；
  每请求媒体块 ≤100；**替换文本必须非空**（否则复刻公开版已知 400
  `cache_control cannot be set for empty text blocks`）；请求体无签名校验
- 识图失败**不能回 4xx**（客户端媒体类 4xx 走 collapse-drain 恢复路径，体验差）

## 2. 决策记录（已拍板）

| # | 决策 | 值 |
|---|------|-----|
| D1 | 支持范围 | Claude（/v1/messages）+ OpenAI（/v1/chat/completions）两套格式都做 |
| D2 | 识图失败降级 | 替换为占位文本 `[Image: description unavailable (原因)]`，继续转发 |
| D3 | 计费 | 阶段 1 网关自担（旁路调用不进 quota 链路） |
| D4 | 分支 | 独立分支 `feat/vision-relay`（本文档所在分支） |
| D5 | 测试环境 | sgp2 的 observer-test 实例（`newapi-test.vectorcontrol.tech`），**复用 observer 的 PG 数据库**；库中已有 deepseek 渠道（上游 api.tokendancelab.com），**用同一个 key 加 gemma 渠道** |

## 3. 现状与链路（代码事实）

### 3.1 请求链路

```
客户端 → POST /v1/messages (Claude) 或 /v1/chat/completions (OpenAI)
  → controller/relay.go:71 Relay → helper.GetAndValidateRequest（反序列化 dto.ClaudeRequest / GeneralOpenAIRequest）
  → token 估算/预扣费 → 渠道选择 retry 循环（controller/relay.go:194-246）
  → relay/claude_handler.go:24 ClaudeHelper（Claude 格式）
       ├─ :34 common.DeepCopy(claudeReq)        ← ★ 改写插入点 A
       ├─ :39 helper.ModelMappedHelper
       ├─ :50-53 DefaultMaxTokens 填充
       ├─ :55-108 thinking adapter（按模型后缀）
       ├─ :110-133 SystemPrompt 注入
       ├─ :135-154 Responses 转换分支
       ├─ :156-163 PassThrough 分支（透传原始 body）
       ├─ :164-198 ConvertClaudeRequest → Marshal → NewOutboundJSONBody
       └─ :202 DoRequest → :218 DoResponse（SSE / 非流式）
  或 relay/compatible_handler.go:25 TextHelper（OpenAI 格式）
       ├─ :33 common.DeepCopy(textReq)          ← ★ 改写插入点 B
       ├─ :42 helper.ModelMappedHelper
       ├─ :97-107 PassThrough 分支
       ├─ :108-186 ConvertOpenAIRequest → SystemPrompt 注入 → Marshal → NewOutboundJSONBody
       └─ :189 DoRequest → :207 DoResponse
  → 上游渠道（40+ 适配器，relay/relay_adaptor.go:41 GetAdaptor 按渠道类型分派）
```

改写点选在 **DeepCopy 之后、格式转换之前**：结构化对象上操作、不碰原始字节、
改写结果自然覆盖下游所有渠道类型、与仓库既有 SystemPrompt 注入/thinking adapter
完全同构（本仓库所有"请求改写"的既定位置）。

### 3.2 关键 DTO（relaykit 独立 module，relaykit/dto/）

- `claude.go`：`ClaudeRequest{Model, System any, Messages []ClaudeMessage}`（:205-236）；
  `ClaudeMessage{Role, Content any}`（:121-124）；`ParseContent()`（:168-170）把 Content
  转为 `[]ClaudeMediaMessage`；`ClaudeMediaMessage{Type, Text *string, Source *ClaudeMessageSource,
  Content any（tool_result 嵌套）, CacheControl json.RawMessage}`（:17-36）；`SetText/GetText`
  （:38-42）；`ClaudeMessageSource{Type, MediaType, Data any, Url string}`（:114-119）；
  `ToFileSource()`（:100-112）；`ParseSystem()`（:480-483，System 可含 image 块）
- `openai_request.go`：`GeneralOpenAIRequest{Model, Messages []Message, Stream *bool}`（:28-109）；
  `Message{Content any}`（:303-314）；`ParseContent() []MediaContent`（:543-674）；
  `MediaContent{Type, Text, ImageUrl any, ...}`（:316-325）；`GetImageMedia() *MessageImageUrl`
  （:327-342）；`MessageImageUrl{Url string}`（:426-430）；常量 `ContentTypeImageURL = "image_url"`
  （:451-458）；`SetMediaContent([]MediaContent)`（:530-533）；`StringContent()`（:497-518）
- `openai_response.go`：`OpenAITextResponse.Choices[0].Message.Content` → `StringContent()`
  取文本（:40-48）

### 3.3 现成可复用设施

| 能力 | 位置 | 说明 |
|------|------|------|
| 图片数据获取 | `service/file_service.go:418 GetBase64Data(c, source, reason)` | 返回 (base64, mimeType)；带 URL 下载 + 磁盘缓存 + SSRF 保护 |
| data URL 解析 | `service/image.go:43 DecodeBase64FileData` | 剥离 `data:<mime>;base64,` 前缀 |
| data URL 组装 | `relaykit/relayconvert/internal/claude_messages/to_oai_chat_req.go:160` | `fmt.Sprintf("data:%s;base64,%s", mediaType, data)` |
| HTTP 客户端 | `service/http_client.go:130 GetHttpClient()` | 无代理通用客户端；注意 `RelayTimeout` 默认 0（无超时，需自建 context） |
| 请求级缓存 | `common/gin.go:157 SetContextKey(c, key, value)` | gin context 跨 relay retry 循环保留（controller/relay.go 同一 `*gin.Context`） |
| 配置注册表 | `setting/model_setting/claude.go:19-51` | ClaudeSettings + `Register("claude")` + `GetClaudeSettings()`，DB options 热加载，JSON tag 序列化，**加字段零 schema 迁移** |
| 网关内调模型先例 | `controller/channel-test.go:76-440`、`relay/chat_completions_via_responses.go:73-150` | 构造 `dto.GeneralOpenAIRequest` → DoRequest 的完整骨架 |

### 3.4 客户端侧硬约束（claude-code 事实，来源：apiLimits.ts:22,29,42-43,94；
imageResizer.ts；claude.ts:3063-3106,588-631,2920-2970；processTextPrompt.ts:66-87；
FileReadTool.ts:652-669）

1. 拦截范围须扫两层：消息 content 数组 + tool_result 嵌套 content
2. 替换为 text 块必须**非空**（400 bug 警示）
3. 被替换块若带 `cache_control`（最后一条消息的最后块会带）必须平移到替换后的 text 块
4. 响应侧：message_delta 必须带 usage（客户端成本统计依赖）；合法 SSE 事件序列；
   缺 message_start 触发客户端自动非流式重试
5. 识图调用必须在转发前完成（预拦截）；客户端 600s 请求超时 + 流空闲 watchdog
6. 客户端本地 token 估算 image=2000 tokens/张——替换后本地统计失真，不报错，可接受

## 4. 设计

### 4.1 总体架构（旁路调用）

```
ClaudeHelper/TextHelper（DeepCopy 后）
  └─ vision.Enhance(c, info, request)          ← 统一入口，默认关闭（VisionEnabled=false 零行为）
       ├─ 扫描 image 块（Claude: type=image / OpenAI: type=image_url，递归 tool_result 嵌套）
       ├─ GetBase64Data 取图（URL 源走 SSRF 保护下载）
       ├─ 压缩大图（>2000px 或 >1.5MB 降采样/转 JPEG，对齐客户端压缩策略）
       ├─ 旁路调用视觉模型（直连 VisionBaseURL，不进 relay 管线）
       │     └─ 不进 relay → 不参与 autogroup/渠道分发/计费/递归——旁路是核心设计
       ├─ 结果替换 image 块 → text 块（保留位置；CacheControl 平移；失败→占位文本）
       └─ SetContextKey 缓存"本请求已识图"（防 relay retry 循环重复调用）
```

### 4.2 配置 — 新文件 `setting/model_setting/vision.go`

仿照 claude.go 模式：

```go
type VisionSettings struct {
    VisionEnabled    bool   `json:"vision_enabled"`     // UI 开关（_enabled 结尾，后台设置页自动渲染）
    VisionModels     string `json:"vision_models"`      // fallback 链，逗号分隔
    VisionBaseURL    string `json:"vision_base_url"`    // 视觉端点（OpenAI 兼容）
    VisionAPIKey     string `json:"vision_api_key"`     // 视觉端点鉴权 key
    VisionTimeoutSec int    `json:"vision_timeout_sec"` // 单图总预算（秒），默认 15
    VisionPrompt     string `json:"vision_prompt"`      // 识图指令模板（默认=vision-bridge 同款保真基线）
    VisionMaxImages  int    `json:"vision_max_images"`  // 单请求最多识图数，默认 6
}
```
+ `defaultVisionSettings` + `config.GlobalConfig.Register("vision", &visionSettings)`
+ `GetVisionSettings()`。消费路径：handler 直接读（无需动 relayconvert convmeta 快照）。

**测试环境值**（D5）：`vision_base_url=https://api.tokendancelab.com`；
`vision_api_key=`（observer 库中 deepseek 渠道同款 key）；`vision_models=gemma-4-31b,...`。

### 4.3 模块结构 — 新包 `relay/vision/`

| 文件 | 职责 |
|------|------|
| `vision.go` | `Enhance(c, info, request) error` 统一入口：开关判断、类型分派、幂等标记 |
| `extract.go` | 扫描+提取：Claude/OpenAI 两格式的 image 块识别（递归 tool_result 嵌套、System 块）、数据获取（GetBase64Data/DecodeBase64FileData）、压缩 |
| `describe.go` | 旁路识图调用：构造 GeneralOpenAIRequest、fallback 链、超时、错误分类（503 model_not_found 永久跳过 / 451 审核 / 瞬时重试）、占位文本生成 |
| `replace.go` | 替换：Claude image 块 → text 块（CacheControl 平移）；OpenAI image_url 块 → text 块；描述非空保证 |
| `vision_test.go` | 单元测试（httptest mock 视觉端点） |

### 4.4 扫描与提取（extract.go）

- Claude 侧：`msg.ParseContent()` 中 `Type=="image"` 且 `Source != nil` → 收集；
  `msg.Content` 为 `[]ClaudeMediaMessage` 且含 `Type=="tool_result"` 的块 → 递归扫其嵌套
  Content；`request.System` 用 `ParseSystem()` 同法处理
- OpenAI 侧：`msg.ParseContent()` 中 `Type=="image_url"` → `GetImageMedia()` 取 url；
  data URL → `DecodeBase64FileData`；http(s) URL → `GetBase64Data`（SSRF 保护下载）
- 统一产出 `VisionImage{Index, Data []byte, MediaType}`；超过 `VisionMaxImages` 的图
  不处理（原样透传）；单图 >15MB 跳过（对齐 vision-bridge 上限）

### 4.5 压缩（extract.go 内）

- 触发条件：边长 >2000px 或原始字节 >1.5MB（对齐 Claude Code 客户端压缩策略）
- 实现：Go 标准库 `image/jpeg` + `golang.org/x/image/draw` 降采样（无 PIL；仓库已有
  x/image 依赖需确认——见 §6 开放问题 Q2）
- 小 PNG（<300KB）无损保留；EXIF 转正（Go 标准库 image 包自动处理大部分）

### 4.6 旁路识图调用（describe.go）

```
callVision(ctx, image, settings) (string, error)
  1. payload = dto.GeneralOpenAIRequest{
       Model: model, MaxTokens: 2000,
       Messages: [{Role:"user", Content: []MediaContent{
         {Type:"text", Text: 指令（VisionPrompt 或默认保真基线）},
         {Type:"image_url", ImageUrl: {Url: "data:<mime>;base64,<data>"}},
       }}],
     }
  2. POST {VisionBaseURL}/v1/chat/completions（GetHttpClient()，context.WithTimeout 总预算）
  3. 解析 OpenAITextResponse.Choices[0].Message.Content → StringContent()
  4. 错误分类：
       - 503 且 body 含 "model_not_found" → 永久跳过该模型（换 fallback 链下一个）
       - HTTP 451 → 内容审核阻断，标记 blocked（占位文本带原因）
       - 其余 5xx/超时 → 瞬时，当前模型尝试内重试 1 次（总预算内）再换下一个
  5. 全部失败 → 占位文本 "[Image: description unavailable (<原因>)]"
```

每图独立调用（多图不合并请求——对齐 vision-bridge 经验：合包触发网关 504）；
识图延迟计入 TTFT（客户端可感知），`VisionTimeoutSec` 是硬预算。

### 4.7 替换（replace.go）

- Claude：image 块 → `ClaudeMediaMessage{Type:"text"}` + `SetText(描述)`；
  **原块 `CacheControl` 平移到新块**；`msg.SetContent([]ClaudeMediaMessage{...})` 回写
- OpenAI：image_url 块 → `MediaContent{Type: ContentTypeText, Text: 描述}`；
  `m.SetMediaContent([]MediaContent{...})` 回写
- 描述为空时用占位文本兜底（保证非空，防 400）

### 4.8 幂等与 retry（vision.go）

- `common.SetContextKey(c, contextKeyVisionProcessed, true)`——relay retry 循环
  （controller/relay.go:194-246）每次重试重新 DeepCopy + 重新进 handler，但 **gin
  context 全程保留**（已核实：同一 `*gin.Context`，键存在 Context 自身）→ 第二次进
  handler 直接跳过识图
- key 定义：`constant/context_key.go` 加 `ContextKeyVisionProcessed`

### 4.9 插入点（每处 2-3 行）

```go
// relay/claude_handler.go:34 之后
request, err := common.DeepCopy(claudeReq)
if err := vision.Enhance(c, info, request); err != nil {
    logger.LogWarn(c, "vision enhance failed: %v", err) // 不阻断主流程
}
```
`relay/compatible_handler.go:33` 之后同构。PassThrough 分支（PassThroughBodyEnabled
开启时结构改写不生效）→ 跳过识图 + LogInfo 记录（阶段 1 最小切面，文档注明）。

### 4.10 日志与可观测

- 每次识图事件：`logger.LogInfo(c, "vision: images=%d recognized=%d failed=%d elapsed=%dms model=%s", ...)`
- 降级原因（timeout/451/no_channel）单独记录，便于诊断
- 阶段 2 可选：接入 relay_observer 审计线元数据（pkg/relay_observer/）

## 5. 可能的问题与风险（重点审核）

| # | 问题 | 影响 | 缓解 |
|---|------|------|------|
| R1 | **识图延迟计入 TTFT**：每带图请求 +1 次上游往返（1-5s） | Claude Code 交互可感知的首次响应变慢 | 默认关闭；VisionTimeoutSec=15 硬预算；超时降级占位文本；只处理 VisionMaxImages 张 |
| R2 | **relay retry 重复识图**：重试循环每次重进 handler | 同一图识别 N 次，浪费配额 | SetContextKey 幂等标记（4.8，机制已核实有效） |
| R3 | **PassThrough 模式失效**：PassThroughBodyEnabled 时透传原始 body | 结构改写不生效，功能静默失效 | 跳过 + LogInfo 记录；阶段 2 用 sjson（仓库已依赖）改原始 body |
| R4 | **替换后下游语义错位**：用户问"describe this image"时模型看到的是描述文本 | 响应与用户意图的错位 | 描述块加前缀标注（如 `[Image N/total 描述]`）；识图指令模板要求 OCR 文字 + 布局保真 |
| R5 | **prompt cache 破坏**：改写 content 改变前缀 | 带图请求首次 cache miss，重新 cache_creation（正常成本） | 可接受；CacheControl 已平移（4.7） |
| R6 | **缓存段数量超限**：被替换块带 cache_control 平移到 text 块后，若该块是最后一块，缓存段数不变 | 无实际风险 | 保持每请求 cache segment ≤4（客户端侧限制） |
| R7 | **视觉端点鉴权与限流**：VisionAPIKey 泄露/失效、端点 429 | 识图失败率高、fallback 链耗尽 | key 存 DB 设置（与渠道 key 同安全级别）；失败降级占位文本，不阻塞主请求 |
| R8 | **上游视觉模型返回非标结构**（如 glm 系 data.choices 非标） | 解析失败误判为识图失败 | 解析兜底：content/reasoning_content 多字段尝试（对齐 vision-bridge 经验） |
| R9 | **大图内存**：单图 base64 可达 5MB、每请求 ≤100 张 | 网关内存峰值 | 只处理前 VisionMaxImages 张；压缩后请求体显著变小 |
| R10 | **SSRF**：OpenAI 格式 image_url 可为 http URL | 网关成为 SSRF 代理 | GetBase64Data 内部已有 SSRF 保护（ValidateSSRFProtectedFetchURL） |
| R11 | **计费语义**：识图消耗不进用户账单 | 网关自担成本；无使用量监控 | 阶段 1 接受；日志记录每次识图事件；阶段 2 可挂 quota |
| R12 | **OpenAI 格式兼容面**：image_url 直接为字符串 vs 对象（ParseContent 两种都兼容，:596-617） | 漏扫 | 按 ParseContent 现有语义处理（已兼容两种） |
| R13 | **与现有 vision-bridge hook 并存**：客户端 hook 已替换的场景，请求无 image 块 → 网关自然放行 | 无冲突 | 无需协调；网关是兜底层 |
| R14 | **多格式入口一致性**：Claude 格式有 System 块图片（少见） | 漏处理 | ParseSystem 同样扫描（4.4） |

## 6. 需要深入研究设计的点（开放问题，请审核给出意见）

- **Q1 压缩实现**：Go 侧无 PIL。方案 A：标准库 `image/jpeg` + `x/image/draw`（若 go.mod
  已有 x/image 依赖则零新增）；方案 B：引入第三方（如 `github.com/disintegration/imaging`）。
  倾向 A（最小依赖）；需确认仓库 go.mod 是否已有 golang.org/x/image。
- **Q2 并发识图**：多图串行（总耗时 N×15s 上限）vs 并发（首图延迟小但瞬时并发高）。
  阶段 1 倾向**串行 + 总预算**（最坏可预测）；阶段 2 可并发 + 每图独立预算。
- **Q3 失败降级语义**：占位文本 vs 原样透传原图——已拍板占位文本（D2）。但细节待定：
  **占位文本是否保留原图信息**（如尺寸/来源路径，供模型判断"图存在但不可读"）？倾向保留。
- **Q4 识图结果缓存**：同图 hash 磁盘缓存（跨请求复用，对话中同一张图反复出现时只识别
  一次）。仓库 `common/disk_cache.go` 是随机文件名的临时文件缓存（:83-93），无 hash 键控
  ——需自建 sha256 文件名键（~30 行）。阶段 1 做还是阶段 2 做？倾向阶段 1 做（对话贴图
  复用频繁，收益明显）。
- **Q5 计费（阶段 2）**：识图消耗计入用户 quota 的挂点在哪（quota 扣减发生在 relay
  主流程 :214-221 PostTextConsumeQuota，旁路调用不进主流程）——需设计旁路消耗的计费
  通道，或维持自担。
- **Q6 前端设置页**：`_enabled` 结尾的 bool 后台自动渲染开关，其余字段（key/URL/models）
  是否要做表单？参照现有设置页模式（前端 electron/ 下设置组件）。
- **Q7 relay_observer 集成**：识图事件是否进审计线（pkg/relay_observer/normalizer.go:798-812
  已有 image 块归一化）——阶段 2 可选。
- **Q8 视觉模型输出上限**：max_tokens 2000 对长表格/长代码截图可能截断（vision-bridge
  实测 step-3.7-flash 内容在 reasoning 字段）。是否需要动态 max_tokens？倾向固定 2000 + 
  reasoning 兜底读取（对齐 vision-bridge 已验方案）。

## 7. 测试与验证

### 7.1 单元测试（relay/vision/vision_test.go，httptest 模式仿
`service/http_client_transport_test.go`）
1. 扫描：Claude/OpenAI 两格式 image 块识别（含 tool_result 嵌套、System 块、url 源）
2. 替换：块类型/位置/CacheControl 平移/非空描述
3. 旁路调用：成功 / fallback 链切换 / 503 model_not_found 跳过 / 超时 / 451 占位
4. 幂等：同请求二次 Enhance 不重复调用
5. 配置：默认关闭零行为；压缩逻辑（大图降采样、小 PNG 保留）
6. 回归：`go build ./...` + `make test`（仓库现有测试不能红）

### 7.2 sgp2 部署测试（复用 observer 数据库，D5）
1. 构建镜像：本地 docker build（Dockerfile：bun web + CGO_ENABLED=0 go build，
   GOEXPERIMENT=greenteagc）或 GitHub Actions 推送 ghcr
2. sgp2 `/opt/observer-test/compose.yml`：observer-test-api 镜像换新镜像；
   **PG/Redis 卷与 DSN 不变（直接复用 observer 数据库）**
3. 设置注册表：`vision_enabled=true`、`vision_base_url=https://api.tokendancelab.com`、
   `vision_api_key=`（deepseek 渠道同款 key）、`vision_models=gemma-4-31b,...`
4. 渠道：复用库中已有 deepseek 渠道；加一条 gemma 渠道（同 key，模型 gemma-4-31b）
5. 端到端：
   - Claude 格式：Claude Code 连 `newapi-test.vectorcontrol.tech`，deepseek 模型贴图
     → 网关替换 → 模型正确描述图片
   - OpenAI 格式：OpenAI 兼容客户端同验
   - 失败降级：错配 VisionAPIKey → 请求仍成功，模型收到占位文本
   - 开关关闭：图片原样透传（回归）
   - retry 幂等：构造一次瞬时渠道失败，确认重试未重复识图（看日志）
6. 观察日志（命中/耗时/降级）；测完恢复原镜像或保留（sgp2 到期 2026-08-15 整体拆除）

## 8. 分阶段落地

| 阶段 | 内容 |
|------|------|
| 0（本文档） | 设计 + 审核（GPT）+ 定稿 |
| 1 | 完整实现（§4 全部）+ 单元测试 + 本地构建 + sgp2 部署验证（§7） |
| 2（可选） | 识图结果 hash 磁盘缓存、并发识图、计费计入 quota、前端设置页、relay_observer 集成、PassThrough sjson 改写 |

## 9. 关键文件索引

- 插入点：`relay/claude_handler.go`（:34 后）、`relay/compatible_handler.go`（:33 后）
- 配置先例：`setting/model_setting/claude.go`（:19-51）
- DTO：`relaykit/dto/claude.go`、`relaykit/dto/openai_request.go`、`relaykit/dto/openai_response.go`
- 数据获取：`service/file_service.go`（:418）、`service/image.go`（:43）
- HTTP：`service/http_client.go`（:130, :366）
- 请求级缓存：`common/gin.go`（:157, :161）、`constant/context_key.go`
- Retry：`controller/relay.go`（:194-246）
- 网关内调模型先例：`controller/channel-test.go`（:76-440）、`relay/chat_completions_via_responses.go`（:73-150）
- data URL 组装先例：`relaykit/relayconvert/internal/claude_messages/to_oai_chat_req.go`（:160）
