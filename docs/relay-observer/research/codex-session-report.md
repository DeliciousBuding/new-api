# Codex agent 会话与请求载荷事实报告

最后更新：2026-08-03

> 来源仓库：`D:\Code\Projects\openai-codex`（OpenAI Codex CLI，Rust）
> 基线：git HEAD `38b064c`（"Render TUI prompts before submitting user turns (#33373)"，含 app-server / protocol v2 / responses-websocket 的新架构）
> 目的：为 relay observer 的 tail-first 内容保留策略与 agentloop 结构化存储提供事实依据。
> 约定：所有证据为 `文件路径:行号`，路径相对于仓库根 `codex-rs/` 前缀省略（`core/src/...` 即 `D:\Code\Projects\openai-codex\codex-rs\core\src\...`）。

---

## 1. 请求载荷形态

### 1.1 请求体结构（Responses API）

请求体 `ResponsesApiRequest` 顶层字段：`model`、`instructions`（字符串，即系统提示词）、`input: Vec<ResponseItem>`（会话全部内容）、`tools`、`tool_choice: "auto"`、`parallel_tool_calls`、`reasoning`、`store`、`stream: true`、`stream_options`、`include`、`service_tier`、`prompt_cache_key`、`text`、`client_metadata`。

- 证据：`codex-api/src/common.rs:216-239`（ResponsesApiRequest 定义）
- 请求体组装：`core/src/client.rs:824-908`（`build_responses_request`）
- `instructions` = `prompt.base_instructions.text`（系统提示词全文）：`core/src/client.rs:862`
- `prompt_cache_key` = session_id：`core/src/client.rs:469-473, 888`
- `include: ["reasoning.encrypted_content"]`（要求响应侧返回加密推理内容）：`core/src/client.rs:871`
- 非 OpenAI 提供商时清空 `internal_chat_message_metadata_passthrough`：`core/src/client.rs:836-840`

### 1.2 instructions（系统提示词）内容与大小

- 默认 base instructions 是单个 markdown 文件，**21,178 字节**（≈5.3k tokens，按 4B/token 估算）：
  - `protocol/src/models.rs:1255-1268`（`BASE_INSTRUCTIONS_DEFAULT = include_str!("prompts/base_instructions/default.md")`）
  - 文件本体：`protocol/src/prompts/base_instructions/default.md`（21,178 字节）
- 优先级：config 的 `base_instructions` 覆盖 > resume 时从会话 meta 恢复 > 模型默认（`model_info.get_model_instructions`）：`core/src/session/mod.rs:594-613`
- `instructions` 字段与 input 的区分：非 lite 模式下 base_instructions 走 `instructions` 字段；input 里另有 role=`developer` 的 Message 承载会话级上下文增量（见 1.4）。
- **Responses Lite 模式**（`use_responses_lite`）：`instructions` 置空，改为把 `additional_tools`（role=developer）与一条含 base_instructions 全文的 developer Message **拼到 input 数组头部**：`core/src/client.rs:842-860`

### 1.3 input items：每次请求都是全量回放，不是增量

- agent loop 每步（每个 sampling 请求）都用 `sess.clone_history().await.for_prompt(...)` 从内存历史**整体重建 input 数组**：`core/src/session/turn.rs:273-279`（run_turn 循环内）、`core/src/session/turn.rs:1162-1168`（run_sampling_request 重试时再次整体重建）
- 历史 `ContextManager.items` 是**旧 → 新**有序向量，`record_items` 只追加到尾部：`core/src/context_manager/history.rs:39, 120-135`
- `for_prompt` 只做归一化（补全 call/output 配对、去掉孤儿 output、按输入模态剥离图片），不截断：`core/src/context_manager/history.rs:141-144, 355-368`
- role=`system` 的 item 不入库（`is_api_message` 排除）：`core/src/context_manager/history.rs:485-505`
- 单轮内多次工具调用的编排：模型返回 `function_call` → 立即记录（`core/src/stream_events_utils.rs:319-357`）→ 工具执行 future 挂到 `in_flight` → 结果（`ResponseInputItem::FunctionCallOutput` 等）`drain_in_flight` 记录进历史尾部（`core/src/session/turn.rs:1908-1928`）→ 下一轮请求 input 尾部出现 `function_call_output`。

### 1.4 item 类型清单与轮次编排

`ResponseItem` 枚举（wire 上为 `type` 标签 + snake_case）：`message` / `agent_message` / `reasoning` / `local_shell_call` / `function_call` / `tool_search_call` / `function_call_output` / `custom_tool_call` / `custom_tool_call_output` / `tool_search_output` / `web_search_call` / `image_generation_call` / `compaction` / `context_compaction` / `additional_tools`（+ `compaction_trigger` 请求控制项、`other` 兜底）。

- 证据：`protocol/src/models.rs:801-1030`
- item ID 前缀（`id_prefix`）：at / msg / amsg / rs / lsh / fc / tsc / fco / ctc / ctco / tso / ws / ig / cmp：`protocol/src/models.rs:1083-1101`
- `function_call.arguments` 是 **JSON 字符串**（不是对象）：`protocol/src/models.rs:863-879`
- `function_call_output.output` 线上为 `content`（纯文本）或 `content_items` 数组两种形态：`protocol/src/models.rs:895-911, 930-947`

典型轮次序列（同一线程内，自首轮起）：

1. 首轮：`[developer message(初始上下文), (contextual user message), user message]` + `instructions` 字段
2. 模型步骤 k：`[…历史…, assistant message, function_call]`（推理条目在 response 侧记录，input 中通常为占位）
3. 工具执行后：`[…, function_call_output(截断后输出)]`
4. 循环至模型输出最终 assistant message 结束 turn

### 1.5 初始上下文（首轮 developer message）构成

`build_initial_context_with_world_state_and_mcp` 把以下 sections 拼进首条 developer message（或独立 developer message）：模型切换提示、权限指令、开发者指令、协作模式指令、realtime 状态、personality、技能清单、插件、token budget 上下文、world-state 全量、扩展贡献的片段等。

- 证据：`core/src/session/mod.rs:3150-3458`
- 首轮注入 vs 稳态 diff：首轮全量注入并建立 `reference_context_item` 基线；后续轮只追加 settings/world-state 的**diff 片段**：`core/src/session/mod.rs:3548-3609`
- 上下文更新 item（developer/user 角色）由 `context_manager/updates.rs` 组装（`build_developer_update_item` / `build_contextual_user_message`）：`core/src/context_manager/updates.rs`
- 用户消息落地：`record_user_prompt_and_emit_turn_item` → 记录 user-role Message：`core/src/session/mod.rs:3787-3799`；hook 注入与 pending input 记录：`core/src/hook_runtime.rs:539-563`

### 1.6 并行工具调用：顺序与配对（邻接不可靠）

- 请求体携带 `parallel_tool_calls: "auto"`（`codex-api/src/common.rs:216-239` 顶层字段，见 1.1）——单次采样响应**可含多个 `function_call`**，逐个立即记录进历史（`core/src/stream_events_utils.rs:319-357`）。
- 多个工具 future 并发挂起于 `in_flight`，结果由 `drain_in_flight` **按完成顺序**追加进历史尾部（`core/src/session/turn.rs:1908-1928`）——**完成顺序 ≠ 调用顺序**，`function_call_output` 与 `function_call` 在数组中的相对位置不可作为配对依据。
- 协议层自己按 `call_id` 配对：`for_prompt` 归一化**补全 call/output 配对、剥离孤儿 output**（`core/src/context_manager/history.rs:141-144, 355-368`）——唯一可靠键是 `call_id`。
- **无"result 后必有 continuation"保证**：turn 以最终 assistant 消息结束时，其后的工具结果/尾部内容不会再出现在任何后续请求中（工具结果只在下一轮请求的 input 尾部可见，见 1.4）。

---

## 2. delta 位置：「最新证据在哪里」

**结论：新内容永远在 input 数组尾部，且每次请求全量回放 → 请求尾部 = 最新证据；截断时保留尾部。**

三条截断/压缩机制及方向：

### 2.1 上下文窗口超限 → 删头保尾

- `ContextWindowExceeded` 时 `history.remove_first_item()` —— 删除最旧 item（index 0），注释明言"preserve cache (prefix-based) and keep recent messages intact"：`core/src/compact.rs:285-294`；实现会顺带删掉配对 call/output 维持不变量：`core/src/context_manager/history.rs:187-198`
- 采样请求遇到 ContextWindowExceeded 不再重试，记录 total_tokens_full 后失败：`core/src/session/turn.rs:1191-1194`

### 2.2 自动压缩（auto-compact）→ 保尾 + 摘要

- 压缩后历史 = **最近 N 条 user 消息（从尾部向前选择，总预算 20,000 tokens）** + **summary（最后一条 assistant 消息全文）**：`core/src/compact.rs:54, 599-660`（`build_compacted_history` 用 `.rev()` 迭代尾部优先，`COMPACT_USER_MESSAGE_MAX_TOKENS = 20_000`）
- summary 取最后一条 assistant message：`core/src/compact.rs:325`；前缀常量 `SUMMARY_PREFIX`（`prompts/templates/compact/summary_prefix.md`，399 字节）
- 压缩请求本身也是一次 Responses 调用（`/responses/compact` 或普通流），`drain_to_completed`：`core/src/compact.rs:662-723`；端点常量 `RESPONSES_COMPACT_ENDPOINT = "/responses/compact"`：`core/src/client.rs:159`
- 压缩后插入 `CompactedItem`（含 replacement_history、window_number、window_id 链）并 `replace_compacted_history`：`core/src/compact.rs:353-368`
- 每次压缩推进上下文窗口：`window_id = "{thread_id}:{window_number}"`：`core/src/session/mod.rs:3474-3479`

### 2.3 工具输出截断 → 同时保留头 + 尾（中段裁掉）

- 单条文本截断 `truncate_middle_chars` / `truncate_middle_with_token_budget`：预算在头部/尾部各分一半，中间插 marker（"… [truncated …] …"）：`utils/string/src/truncate.rs:38-68`（`split_budget` / `split_string`）
- 记录进历史时按 `truncation_policy * 1.2` 截断 `FunctionCallOutput` / `CustomToolCallOutput`：`core/src/context_manager/history.rs:370-413`
- 截断带警告前缀 `Warning: truncated output (original token count: N)\nTotal output lines: M`：`utils/output-truncation/src/lib.rs:12-30`
- exec 输出格式（发给模型的 function_call_output 文本）：`Exit code: X` / `Wall time: Y seconds` / `Total output lines: Z` / `Output: …`：`core/src/tools/mod.rs:78-113`
- 截断策略来源：模型元数据 `truncation_policy`（fallback `bytes 10_000`）：`models-manager/src/model_info.rs:151`；config 可覆盖 `tool_output_token_limit`：`config/src/config_toml.rs:298-299`、`models-manager/src/model_info.rs:35-50`

---

## 3. session 与分支

### 3.1 存储与文件命名

- 目录布局：`~/.codex/sessions/YYYY/MM/DD/rollout-YYYY-MM-DDThh-mm-ss-<thread_id>.jsonl`：`rollout/src/recorder.rs:1505-1530`（`precompute_log_file_info`）；文档注释 `rollout/src/list.rs:420`
- 文件名中的 `-<thread_id>` 即线程 UUIDv7，是解析会话 id 的 canonical 来源：`rollout/src/list.rs:964-967`
- 压缩归档：`~/.codex/archived_sessions/`：`rollout/src/lib.rs:24-25`
- 会话索引：`~/.codex/session_index.jsonl`（追加式 `{id, thread_name, updated_at}`，按 id 从末尾扫描取最新）：`rollout/src/session_index.rs:19-27, 105-119`
- 另有 SQLite 镜像：`state_5.sqlite` / `logs_2.sqlite` / `thread_history_1.sqlite` / `goals_1.sqlite` / `memories_1.sqlite`：`state/src/lib.rs:100-104`

### 3.2 JSONL 行结构

每行一个 `RolloutItem`（`type` + `payload`）：`session_meta` / `response_item` / `inter_agent_communication` / `inter_agent_communication_metadata` / `compacted` / `turn_context` / `world_state` / `event_msg`。

- 证据：`protocol/src/protocol.rs:3166-3179`
- 首行 `session_meta` 字段：`session_id`、`id`(thread_id)、`forked_from_id`、`parent_thread_id`、`timestamp`、`cwd`、`originator`、`cli_version`、`agent_nickname/role/path`、`source`、`thread_source`、`model_provider`、`base_instructions`（全文）、`dynamic_tools`、`selected_capability_roots`、`memory_mode`、`history_mode`、`multi_agent_version`、`context_window`：`rollout/src/recorder.rs:792-818`；结构 `protocol/src/protocol.rs:3048-3133`
- `compacted` 条目携带 `replacement_history`（压缩后的新历史全文）与窗口链 `window_number` / `first_window_id` / `previous_window_id` / `window_id`：`protocol/src/protocol.rs:3199-3216`

### 3.3 session / thread / turn 标识

- `ThreadId`、`SessionId` 均为 **UUIDv7**（含时间戳）：`protocol/src/thread_id.rs:16-25`、`protocol/src/session_id.rs:15-24`
- 根线程：`session_id = SessionId::from(thread_id)`（同一 UUID）；子 agent（subagent）继承父 `agent_control.session_id()`；resume 时从 rollout session_meta 恢复 session_id：`core/src/session/session.rs:536-547`
- turn id（`sub_id`）= 每次提交生成的 UUIDv7：`core/src/session/mod.rs:820-822`（注释：公开 turn id 即此 UUIDv7）
- 请求元数据全量进 `client_metadata` 与头部（见 §4）

### 3.4 resume

- 启动时 `InitialHistory::Resumed`：从 rollout 文件按 **最新 surviving `compacted.replacement_history` 为基底** + 其后存活尾部逐条重放，重建历史、previous_turn_settings、reference_context_item、world_state baseline：`core/src/session/rollout_reconstruction.rs:113-440`
- 逆向扫描（newest→oldest），找到基底后停止：`rollout_reconstruction.rs:154-295`；rollback 语义在重放中折算（跳过被回滚的 user turn）：`rollout_reconstruction.rs:188-191, 365-367`
- 恢复历史同样按 truncation policy 截断工具输出：`rollout_reconstruction.rs:325-339`

### 3.5 分支概念：fork / rollback（rewind）均有实现

- **Fork**：一等公民。`InitialHistory::Forked` 拷贝源线程 rollout 重建历史：`core/src/session/mod.rs:1303-1313`；`forked_from_id` 记录在 SessionMeta 与请求元数据（`responses_metadata.rs:35, 161`）
- fork 边界判定 = 真实 user turn 边界 + `trigger_turn` 的 agent 间通信：`core/src/thread_rollout_truncation.rs:62-126`
- 按 turn id 切割工具：`truncate_rollout_before_turn_id` / `truncate_rollout_after_turn_id`：`core/src/thread_rollout_truncation.rs:160-235`
- **Rollback（rewind）**：`ThreadRolledBack { num_turns }` 事件 → `drop_last_n_user_turns`（删尾部最近 N 个用户轮次，保留头部 pre-turn 上下文）：`core/src/context_manager/history.rs:240-279`；rollout 索引同样折算：`core/src/thread_rollout_truncation.rs:39-60, 105-121`
- **保留尾部 N 个 fork turn**：`truncate_rollout_to_last_n_fork_turns`（`n_from_end`，从后往前保留）：`core/src/thread_rollout_truncation.rs:237-259`

---

## 4. UA 识别

### 4.1 User-Agent 构造

格式字符串：

```
"{originator}/{CARGO_PKG_VERSION} ({os_type} {os_version}; {arch}) {terminal_token}"
```

- 证据：`login/src/auth/default_client.rs:141-152`（`get_codex_user_agent`）；`CARGO_PKG_VERSION` 来自 crate 版本
- 例：`codex_cli_rs/0.50.0 (Windows 11; x86_64) Windows Terminal/1.20.1`
- `terminal_token` = 终端程序/版本或 TERM（sanitized）：`terminal-detection/src/lib.rs:176-189, 275-278`
- 默认 originator：`"codex_cli_rs"`：`login/src/auth/default_client.rs:42`；可用环境变量 `CODEX_INTERNAL_ORIGINATOR_OVERRIDE` 覆盖：`:43, 65-84`
- User-Agent 附加到默认客户端：`login/src/auth/default_client.rs:334-339`

### 4.2 独立请求头（请求侧可观测的全部标识）

| Header | 值 | 证据 |
|---|---|---|
| `originator` | originator 值（`codex_cli_rs` 默认不发送——等于默认值则跳过；非默认才发） | `login/src/auth/default_client.rs:336`；`core/src/client.rs:1887-1901` |
| `x-codex-installation-id` | 持久 UUIDv4（`~/.codex/installation_id` 文件） | `core/src/client.rs:141, 598-601`；`core/src/installation_id.rs:17-64` |
| `x-codex-turn-metadata` | 完整 JSON（见下） | `core/src/client.rs:143`；`core/src/responses_metadata.rs:243-256` |
| `x-codex-window-id` | `"{thread_id}:{window_number}"` | `core/src/client.rs:145`；`core/src/session/mod.rs:3474-3479` |
| `x-codex-parent-thread-id` | 父线程 UUID（子线程） | `core/src/client.rs:144` |
| `x-openai-subagent` | `review` / `compact` / `memory_consolidation` / `collab_spawn` 等 | `core/src/client.rs:147`；`core/src/responses_metadata.rs:305-324` |
| `x-openai-memgen-request` | `true`（memory 整合请求） | `core/src/client.rs:146, 732-736` |
| `x-client-request-id` | thread_id（websocket 握手） | `core/src/client.rs:1081-1083` |
| `OpenAI-Beta` | `responses_websockets=2026-02-06` | `core/src/client.rs:140, 154, 1092-1095` |
| `x-codex-beta-features` / `x-codex-turn-state` | 特性开关 / 传输层 turn 状态 | `core/src/client.rs:1870-1885` |
| `x-responsesapi-include-timing-metrics` | `true`（开 timing 时） | `core/src/client.rs:148-149, 1096-1101` |
| `x-openai-internal-codex-responses-lite` | `true`（responses lite 时） | `core/src/client.rs:155-156, 1903-1910` |
| `x-oai-attestation` | attestation（支持的提供商） | `core/src/client.rs:612-614` |

### 4.3 `x-codex-turn-metadata` / `client_metadata` 内容

`client_metadata["x-codex-turn-metadata"]` 是 canonical 载体（其余为兼容投影）：`core/src/responses_metadata.rs:147-152, 210-241`。

载荷字段（`CodexTurnMetadataPayload`）：`installation_id`、`session_id`、`thread_id`、`turn_id`、`window_id`、`request_kind`（`turn`/`prewarm`/`compaction`/`memory`）、`forked_from_thread_id`、`parent_thread_id`、`subagent_kind`、`thread_source`、`sandbox`、`workspaces`（git 元数据：远程 URL、HEAD commit、dirty 标志）、`turn_started_at_unix_ms`、`compaction`（trigger/reason/implementation/phase/strategy）、自定义 `extra`。

- 证据：`core/src/responses_metadata.rs:270-302, 358-390`；workspace git 元数据：`core/src/turn_metadata.rs:315-328`
- `client_metadata` 平铺键：`x-codex-installation-id`、`session_id`、`thread_id`、`x-codex-window-id`、`turn_id`、`x-openai-subagent`、`x-codex-parent-thread-id`、`x-codex-turn-metadata`：`core/src/responses_metadata.rs:210-241`

---

## 5. 输出捕获机会：请求侧 vs 响应侧

### 5.1 请求侧（input）可见

- 工具调用参数：`function_call.arguments`（JSON 字符串）、`custom_tool_call.input`：`protocol/src/models.rs:863-879, 912-929`
- 工具输出：`function_call_output` / `custom_tool_call_output` / `tool_search_output` —— **已按截断策略处理后的文本**（head+tail 保留 + 中段 marker + `Warning: truncated output` 前缀）：`core/src/context_manager/history.rs:370-413`；`utils/output-truncation/src/lib.rs:12-30`
- exec 类输出带 `Exit code:` / `Wall time:` / `Total output lines:` 头：`core/src/tools/mod.rs:78-103`
- 用户消息原文、assistant 最终文本（在后续请求 input 中）、web_search_call 的 query、图像生成调用

### 5.2 响应侧独有（只捕获请求必然缺失）

1. **未截断的原始工具输出** —— 请求侧只有截断后的 head+tail。最可惜项。
2. **推理内容** —— 响应侧 `include: ["reasoning.encrypted_content"]`（加密）：`core/src/client.rs:871`；请求 input 中的 reasoning 条目不携带明文。
3. **token 用量** —— `ResponseEvent::Completed` 的 `token_usage`（input/cache_read/output/reasoning）：`core/src/session/turn.rs:2293-2316`
4. **流式过程** —— `OutputTextDelta` / `ReasoningSummaryDelta` / `ToolCallInputDelta`（时序信息）：`core/src/session/turn.rs:2329-2450`
5. **assistant 消息 phase**（commentary / final_answer）与 `response_id`（`RawResponseCompleted`）：`core/src/session/turn.rs:2305-2312`
6. **速率限制快照**（`ResponseEvent::RateLimits`）：`core/src/compact.rs:699-701`
7. 缓存命中（usage 的 cache_read 部分）—— 只有响应侧有。

### 5.3 审计建议对照

- 只捕请求 + tail-first 保留，能完整还原：指令、工具调用序列、截断后工具输出、最终文本、会话/线程/轮次标识链（client_metadata）。
- 会永久缺失：原始工具输出、推理明文、token 计量、流式时序 —— 如需，须在响应侧旁路（如 relay 同时观察 response 流）或显式接受缺失。

---

## 6. 对 tail-first 截断策略有直接影响的 5 条事实

1. **请求是全量回放，最新证据恒在 input 尾部**。每个 agent-loop 步骤都整体重建 input（`core/src/session/turn.rs:273-279, 1162-1168`），新内容（用户消息、function_call_output、assistant 消息）只追加尾部（`core/src/context_manager/history.rs:126-135`）。tail-first 保留 = 覆盖每个请求的最新增量，且无跨请求拼接需求。

2. **Codex 自己的上下文回收也是 tail-first**。窗口超限时删最旧 item（`core/src/compact.rs:285-294`、`core/src/context_manager/history.rs:187-198`），auto-compact 从尾部向前保留 user 消息（20k token 预算）+ 最后 assistant 消息摘要（`core/src/compact.rs:54, 599-660`）。审计层采用同样的"保尾"方向与代码库行为一致，最旧证据在 Codex 侧本来就会被先淘汰。

3. **但单条工具输出的截断是"保头+保尾"而非纯保尾**。`truncate_middle` 把预算平分给首尾、裁掉中段（`utils/string/src/truncate.rs:38-68`）。若 relay 只存请求尾部，会看到同一输出的头尾两段 + `Warning: truncated output` 标记，中段不可恢复 —— 需要把"截断后的头"与"截断后的尾"拼接语义写进存储模型，或按 `call_id` 关联 function_call_output 与下一条响应。

4. **压缩事件会重置证据链**。auto-compact 后 input 中的旧轮次被 `replacement_history`（最近 user 消息 + summary）替换，`window_id` 递增（`core/src/compact.rs:353-368`、`core/src/session/mod.rs:3474-3479`）。审计侧遇到 `compaction`/`context_compaction` item 或 `x-codex-window-id` 变化，应视为"前文已被摘要化"，按窗口分段归档，避免把摘要误当原文。

5. **请求自带完整会话标识链，可零解析做关联**。`client_metadata`/头部携带 `installation_id`、`session_id`、`thread_id`、`turn_id`、`window_id`、`request_kind`、`forked_from_thread_id`、`parent_thread_id`、`subagent_kind`、`sandbox`、workspace git 元数据（`core/src/responses_metadata.rs:210-241, 358-390`）；UA 格式固定可解析出 CLI 版本（`login/src/auth/default_client.rs:141-152`）。agentloop 结构化存储可直接以这些字段为键，无需读 `~/.codex/sessions/*.jsonl`；JSONL 侧（`rollout/src/recorder.rs:792-818`）仅在需要工具输出原文的响应侧信息时才值得回读。

---

## 附：关键文件索引（仓库根 `D:\Code\Projects\openai-codex\codex-rs\`）

| 主题 | 文件 |
|---|---|
| 请求体组装 / 头部 | `core/src/client.rs`、`core/src/responses_metadata.rs` |
| Prompt 结构 | `core/src/client_common.rs:16-49`、`codex-api/src/common.rs:216-263` |
| 历史管理（追加/删除/截断） | `core/src/context_manager/history.rs` |
| 压缩 | `core/src/compact.rs`、`prompts/templates/compact/` |
| 轮次循环 / 流处理 | `core/src/session/turn.rs`、`core/src/stream_events_utils.rs` |
| 会话启动 / 初始上下文 | `core/src/session/mod.rs`、`core/src/session/session.rs` |
| rollout 写入与文件名 | `rollout/src/recorder.rs`、`rollout/src/session_index.rs` |
| rollout 重建（resume/fork） | `core/src/session/rollout_reconstruction.rs`、`core/src/thread_rollout_truncation.rs` |
| 模型/协议模型 | `protocol/src/models.rs`、`protocol/src/protocol.rs` |
| UA / originator | `login/src/auth/default_client.rs`、`terminal-detection/src/lib.rs` |
| 输出截断 | `utils/string/src/truncate.rs`、`utils/output-truncation/src/lib.rs`、`core/src/tools/mod.rs` |
