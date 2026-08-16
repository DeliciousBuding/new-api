# Billing Safety — 实现细节与审计埋点

新增计费乘数、quota 转换、媒体计费或旁路参数时阅读本文；不可产生负扣费等硬边界以 `AGENTS.md` 为准。

## 边界常量（新增 relay format / DTO 时复用）

- 图片生成 count：`dto.MaxImageN`
- 视频时长：`relaycommon.MaxTaskDurationSeconds`
- `max_tokens` 家族（OpenAI/Claude/Gemini/Responses 全格式）：`maxTokensLimit`（`relay/helper/valid_request.go`）
- 禁止为同一概念另起临时上限；新 format/DTO 从第一天就约束其 max-tokens 与 count 字段。

## 校验旁路（必须就地补边界）

passthrough 字段（如 `Extra["parameters"]`）、task `metadata` maps、multipart form fields 都能携带同样的量绕过标准 DTO 校验；任何从这些路径读乘数的适配器必须就地执行同样的 bound（或 clamp）。

## 媒体元数据时长饱和

音频文件头（transcription token 计数、TTS 响应时长）与上游扣减数字（如 Kling `FinalUnitDeduction`）都可声明荒谬值；先饱和转换，再进入 token 计数。

## quota 算术中心化（`common/quota_math.go`）

- 禁止裸 cast：`int(float64(quota) * ratio)`、无界输入的 `int(math.Round(...))`、`int(decimal.IntPart())`。
- 使用：`common.QuotaFromFloat`（截断，float 乘积）、`common.QuotaRound`（half-away-from-zero，四舍五入语义）、`common.QuotaFromDecimal`（decimal 乘积）。
- `billingexpr.QuotaRound` 委托 `common.QuotaRound`；禁止重引本地转换 helper 或裸 cast。
- 饱和界为 int32（user/token/log quota 列是 32 位整数）；每个 clamp/NaN fallback 经 `common.SysError` 记录。

## 饱和审计链（Checked 变体 + admin_info 埋点）

- `common.QuotaFromFloatChecked` / `QuotaRoundChecked` / `QuotaFromDecimalChecked` 在 clamp 时额外返回 `*common.QuotaClamp`。
- 计费路径计算 charge 时捕获 clamp → `relayInfo.QuotaClamp`（或线程入 task settlement）→ 写 consume/task log 前调 `attachQuotaSaturation`（`service/log_info_generate.go`），将标记嵌到 log 的 `other.admin_info.quota_saturation` 并 emit request-correlated `logger.LogWarn`。
- `admin_info` 下嵌套使该标记默认仅 admin 可见（非 admin log 视图剥离 `admin_info`）。新增计费路径必须用 `*Checked` 变体并按同样方式暴露 clamp。

## 乘数 map 与预扣/结算

- 乘数 map 只经 `types.PriceData.AddOtherRatio`（拒绝非正 / NaN / +Inf）；禁止直写 `PriceData.OtherRatios` 或削弱守卫。
- 预扣费（预扣费）与结算（结算/差额）都须安全：饱和超量 quota 必须 insufficient-quota 失败，禁止静默回绕。
- 新增计费路径（新 relay format / task 平台 / adjustment hook）时全链追溯：validation → EstimateBilling/OtherRatios → quota 转换 → pre-consume → settle/refund，确认每步不变量。

## 无符号字段上界

`*uint` 字段可接受巨大正 JSON 数（如 `18446744073686646784`，回绕后的负数）；`>= 0` 检查不够，必须显式上界。

## 回归测试位置

边界归属哪个组件，测试就放哪：request validators（`relay/helper/openai_image_request_test.go`）、converter（`relay/common/relay_utils_test.go`）、`common/quota_math_test.go`。
