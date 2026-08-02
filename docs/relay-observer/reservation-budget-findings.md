# 证据层优化：截断根因实测报告（设计输入）

日期：2026-08-03
位置：pkg/relay_observer 预算策略设计的实证输入（配合 claude-code/codex 源码研究报告使用）

## 0. 结论速览

> 状态：P0-B 预算拆解已实现（2026-08-03，PR #12）：capture limit 与 admission 分离
> （`RELAY_OBSERVER_MAX_CAPTURE_BYTES_PER_TURN`），最小 envelope 保证 gap marker
> 可容纳（`RELAY_OBSERVER_MIN_CAPTURE_ENVELOPE_BYTES`）。session/content 解耦
> （P0-A）与锁序统一（T3）同期完成。剩余：head-anchor + tail-biased selector（P0-C，
> 等源码研究报告）。

1. **内容捕获与会话追踪被同一个根因杀死**：canonical 膨胀使预算（=body size）永远不够 → 小请求 items 全空 → `planContent` 的 `len(plan.items)==0` 短路 → **身份解析、session 创建、内容写入全部跳过**。
2. 身份识别链本身**正常**（实弹验证：X-Codex-Turn-Metadata 正确解析为 codex_cli scope 别名）。
3. 修复优先级 = P0：预算口径修正 + tail-first 保留（不是"大请求优化"，是"一切请求的捕获救回"）。

## 1. 实弹数据（sgp2 测试站 newapi-test.vectorcontrol.tech）

| 请求 | body 大小 | agent 头 | 结果 |
|------|----------|---------|------|
| 既有 11 个 turn（历史测试） | ~110-200B | 无 | 全部 gap、9/11 无 session |
| probe 1（chat 1 message） | ~110B | 无 | 200 成功；gap；无 session；无 content |
| probe 2（codex 头 ×2 + claude 头 + plain，4 个） | ~110B | 有（codex/claude 头） | 全 200；全 gap；**全无 session** |
| probe 3（40 messages + prompt_cache_key） | 4071B | X-Codex-Turn-Metadata | 200；gap；**session 创建 ✓**；alias=`codex_cli`（turn_thread/turn_session/prompt_cache_key 三个 source 全部正确解析） |
| 修复后验证（2026-08-03，部署 S2/S3/S4 后） | big 30 msg / small 1 msg | codex 头 | big：200/gap/session ✓；small：200/gap/无 session——与修复前一致，无回归；截断行为属 tail-first 修复范围 |

## 2. 根因链（源码证据）

1. `publishTurnEvent`（service/relay_observation.go:271）：`reservation = BodyStorage.Size()`（真实 body 大小，非 0）。
2. `finishNormalize`（pkg/relay_observer/normalizer.go:183）：`limit = min(reservation, maxRequestBytes)`，head-first 从第一个 item 累加，超限 break。
3. **膨胀定量**：每个 canonical item 带 `hmac`（64 hex + 2 引号 ≈ 66B）+ `kind/role/content/logical_bytes` 字段结构；实测 body 110B 的请求第一个 message item canonical ≈ 200B → **膨胀 ~1.8×** → 第一个 item 就超预算 → items 空（gap marker 也放不下）→ ContentState=Gap。
4. **短路**（pkg/relay_observer/dispatcher.go:681）：`if len(plan.items) == 0 { continue }`——身份解析在 items 检查**之后**，items 空时永不执行 → 无 session、无 content。
5. 大请求（4KB body）：40 个 item 的 canonical ≈ 8KB > 4KB → 头部 item 写入、尾部截断 → items 非空 → 身份解析运行 → session 创建 ✓（对照实验证明身份链正常）。

## 3. 对 tail-first 预算策略的设计约束（待源码研究报告确认后细化）

1. **预算口径**：`reservation = body size` 对 canonical 存储而言系统性不足。选项：
   - a) 预算乘膨胀系数（实测 ~1.8×，建议 `min(body×2, maxRequestBytes)`）；
   - b) 至少保留**最后一个 item**（最新证据）的兜底规则；
   - 倾向：b 为兜底 + a 为主口径，需与存储上限（MaxRequestBytes、队列字节预算）核对。
2. **tail-first**：从尾部 item 起保留（agent 的最新 tool 循环产物在尾部），头部压缩为 gap marker（droppedLogical + 自身 digest，不破坏 dedup 语义）。
3. **短路保护**：`planContent` 的 items 空检查应区分"真空请求"（metadata-only）与"截断空"（gap 状态）——gap 状态且 items 空时仍应尝试身份解析（让 session 至少被追踪），或由预算修复自然解决。
4. **gap marker 的位置语义**：tail-first 后 gap marker 在头部；content.go 的 group 方案（digest 列表含 marker 任意位置）已兼容 ✓（marker 是普通 item，commonPrefix 按 digest 稳定计算）。
5. **ScopeUnknown 请求**（无 agent 头）：即使修复预算，普通 API 用户请求仍无 session（设计如此——会话身份链只有 agent 有）。产品决策：是否对无身份请求启用"credential 维度"兜底 session（SSOT 已有 SourceCredential 设计，当前未用）——列入 P1 讨论。

## 4. 验证方法（修复后）

```bash
# 小请求（~110B chat）应：200、session 创建（若带 agent 头）、content 捕获（至少 1 个 message item + 无 gap）
# 大请求（~4KB，40 msg）应：尾部 item 保留、头部 gap marker
# 带 X-Codex-Turn-Metadata 请求：codex_cli scope、turn_thread/turn_session/prompt_cache_key 三个别名
# 对照：plain 请求无 session（ScopeUnknown 设计行为）
```
