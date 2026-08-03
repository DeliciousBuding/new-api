# P0-C 设计：head-anchor + tail-biased 语义单元选择

日期：2026-08-03
状态：设计定稿，待实现
依据：`research/codex-session-report.md` + `research/claude-code-session-report.md`（源码事实）+ `reservation-budget-findings.md`（实弹数据）+ P0-B 已实现的预算拆解

## 0. 设计结论速览

1. **tail-first 正确性由源码确认**：两个 agent 均全量回放（compact boundary 后全部消息），新证据恒在数组尾部 → 保尾 = 保最新证据，截头不丢任何未归档新内容。
2. **head-anchor 不需要特殊存储**：system/developer 指令每请求重发且同会话近似不变（claude ~50-70K tokens、codex 21KB instructions）→ content object 的 HMAC dedup 自动去重，跨请求只存一次。
3. **语义单元 = tool 配对**：claude `tool_use_id` / codex `call_id` 跨请求配对（tool_call 在请求 N、tool_result 在请求 N+1）——选择器必须保持配对或显式 gap。
4. **compact 边界 = 逻辑重载点**：codex `window_id` 链 / claude `compact_boundary` marker——审计侧识别并分段归档，避免把摘要当原文。
5. **实现为 v1 简化版**：tail-biased 逆序选择 + 配对保护 + 头部 gap；接受 context suffix 放大（方案 A），以固定 corpus benchmark 验证。

## 1. 源码事实（交叉确认）

| 事实 | codex 证据 | claude 证据 |
|------|-----------|------------|
| 全量回放 | `core/src/session/turn.rs:273-279` 每步整体重建 input | `src/query.ts:365,660` 全量 messages 进每次调用 |
| 新内容恒在尾部 | `context_manager/history.rs:126-135` 只追加尾部 | `query.ts:1715-1717` `旧+assistant+tool_results` 尾部拼接 |
| 自身回收也是保尾 | `compact.rs:285-294` 删头保尾；auto-compact 尾部 20k token + 摘要 | auto-compact 数组替换为 boundary+summary；缓存点 `cache_control` 只打最后一条（`claude.ts:3089`） |
| 工具输出截断 | 保头+保尾中段裁（`utils/string/src/truncate.rs:38-68`） | 本地执行后包装为 tool_result，只在下一次请求尾部出现 |
| tool 配对键 | `call_id`（function_call ↔ function_call_output） | `tool_use_id`（tool_use ↔ tool_result，`toolExecution.ts:396-408`） |
| compact 标识 | `compaction`/`context_compaction` item + `x-codex-window-id` 变化 | `compact_boundary` system 消息 + `logicalParentUuid` |
| 系统提示词 | `instructions` 字段 21KB + developer message 首轮全量后续 diff | system 字段每请求重发 ~50-70K tokens，同会话近似不变 |
| 分支 | fork/rollback/rewind 一等公民（`InitialHistory::Forked`、`ThreadRolledBack`） | /branch 全量拷贝+forkedFrom、/rewind 同 session 截短、`parentUuid` 链 |
| 完整标识链 | `x-codex-turn-metadata`（installation/session/thread/turn/window/request_kind/forked_from/parent_thread/subagent_kind/sandbox/workspace） | `X-Claude-Code-Session-Id` + `metadata.user_id` JSON + `parentUuid`/`isSidechain`/`logicalParentUuid` |
| 响应侧独有（请求捕获缺失） | 未截断原始工具输出、推理明文（加密）、token usage、流式时序 | usage、stop_reason、message.id（requestId 可随下轮回放恢复） |

## 2. 选择策略（v1）

在 `finishNormalize` 中实现（P0-B 已提供 selectionBudget = limit - envelope）：

### 2.1 tail-biased 逆序选择

```
从尾部向前选择 item，直到 selectionBudget 耗尽
被跳过的头部合并为一个 gap marker（position=head，droppedLogical + 自身 digest）
items 以原始顺序写回（头部 gap + 保留的尾部）
```

- 与 Codex/Claude 自身回收方向一致（生态同向，最旧证据先淘汰）
- 全量回放意味着每 turn 请求的尾部 = 该轮增量 → 保留的尾部天然覆盖最新证据链

### 2.2 配对保护（语义单元不可拆散）

选择时维护配对集合：
- claude：`tool_result`（带 tool_use_id）→ 若其 `tool_use` 在预算内则一并选择；若 tool_use 已被跳过（在 gap 区），tool_result 保留但标注 `paired_gap`（明确缺失配对，不假装完整）
- codex：`function_call_output`（带 call_id）→ 同规则

实现：逆序选择时遇到 tool_result 检查其 call/tool_use 是否在"已选择"或"可及"（预算允许）——v1 简化：**tool_result 与其 tool_call 视为一个选择单元**（两者的 canonical 总大小进预算），保持配对完整性优先。

### 2.3 head-anchor

- system/developer 指令**不特殊保留**：HMAC dedup 保证同会话跨请求只存一次 content object；但每个 turn 的 digest 列表仍含 system item（预算占用）。v1 决策：**system item 纳入正常选择**（它在头部，tail-biased 下通常被 gap 覆盖——因为同会话每轮相同，dedup 后重建时从历史对象恢复，不丢失信息）。
- **风险**：跨 turn 重建时 delta 的 commonPrefix 若以 system 开头而 system 在 gap 区……（见 §4）

### 2.4 compact 边界识别（归档分段）

- codex：canonical item 中出现 `compaction`/`context_compaction` kind，或请求头 `x-codex-window-id` 变化 → 归档旧窗口（查询 API 已按 turn 分段，只需在内容重建时识别窗口边界）
- claude：`compact_boundary` system 消息（type=system, subtype=compact_boundary）→ 归档边界
- v1：normalizer 对这两类 item 保持透传（kind=unknown 或专门 kind），查询层提供窗口过滤；**不做自动分段存储**（v2 考虑）

## 3. Gap marker 扩展

现有 marker：`{kind:"gap", logical_bytes, truncated, hmac}`。增加：

```json
{
  "kind": "gap",
  "position": "head",          // head | middle | tail（v1 只有 head）
  "omitted_items": 37,
  "logical_bytes": 182344,
  "truncated": true,
  "hmac": "..."                // marker 自身内容 digest（保持不碰撞语义）
}
```

`position` 与 `omitted_items` 使重建端能区分"截头"与"协议未知"。

## 4. 权衡与风险

### 4.1 context suffix 放大（GPT 第七节方案 A）

纯 tail 窗口随对话增长滑动，commonPrefix 在 gap 处断裂 → delta 的 suffix 几乎全量 → context 元数据放大。**v1 接受**，验收以 corpus benchmark 量化：
- capture recall（最新消息保住率）
- context storage amplification（suffix bytes / full bytes 比）
- content-object dedup ratio（跨 turn 复用率）
- common_prefix_count 分布

**缓解观察**：头部 gap marker 的 digest 是 marker 自身内容的稳定 digest（不随 omitted 变化？——omitted_items/logical_bytes 随每轮变化 → marker digest 每轮不同 → commonPrefix 在 marker 处断裂确认）。**方案 B（prefix+suffix 双端 delta）** 记为 v2 候选，不在 v1 实现。

### 4.2 配对单元大小

tool_result 可能超大（工具输出）→ 配对单元超预算时：**降级为"保留 tool_result 的截断头部 + gap"**（与 codex 自身 truncate_middle 语义一致），并标注 `paired_gap`。

### 4.3 已知局限（请求侧捕获固有）

- 原始工具输出中段不可恢复（codex 保头+保尾截断；claude tool_result 在下次请求尾部）
- usage/stop_reason/推理明文只响应侧有 → 如需须响应侧旁路（v2 评估）

## 5. 验收

```bash
# 单元：配对保护 / tail 选择 / gap position / marker 预算
go test ./pkg/relay_observer/ -run 'TestNormalizerTail|TestTailBiased|TestGapPosition' -count=1

# corpus benchmark（固定 corpus：4 个 agent fixture + 合成滑动窗口）
go test ./pkg/relay_observer/ -bench 'BenchmarkTailSelector' -benchtime=200x

# 集成（真实 PG）：tail 截断 + 重建 + 配对标注
go test -tags relay_observer_pg_integration ./pkg/relay_observer/ -run TestIntegrationTail -count=1
```

**接受标准**：最新 user 消息 + 其 tool 链 100% 保住（预算 ≥ 单元大小）；gap marker 恒存在；重建无死链。
