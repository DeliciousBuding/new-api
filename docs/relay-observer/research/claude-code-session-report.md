# Claude Code agent 会话与请求载荷事实报告

最后更新：2026-08-03

> 调研对象：`D:\Code\Projects\claude-code`（Claude Code TypeScript 源码，~1900 文件，Bun 运行时；注意：任务描述称 Go 源码，实际为 TypeScript）。
> 用途：relay observer 的 tail-first 内容保留策略与 agentloop 结构化存储设计依据。
> 方法：全部结论附 `文件:行号` 证据。仓库根路径下文省略为 `<repo>`。
> 关键事实先列：**单轮 agent loop 内每个 API 请求 = 全量重放（full replay）整个 compact-boundary 之后的对话**；**每次请求都在尾部追加增量**；**tool 执行输出只出现在下一次请求的 tool_result 里，响应流里没有**。

---

## 1. 请求载荷形态

### 1.1 请求入口与 body 组装

- 主线程每轮 prompt 提交调用 `query()`，传入**全量** `messages`（REPL 侧 `messagesRef.current` 经 `messagesIncludingNewMessages` 传入）：`<repo>/src/screens/REPL.tsx:2793-2801`。
- `query()` 内的 agent loop（`queryLoop`）每轮迭代把消息复制一份再发请求：
  - `<repo>/src/query.ts:365` — `let messagesForQuery = [...getMessagesAfterCompactBoundary(messages)]`
  - `<repo>/src/query.ts:659-660` — `deps.callModel({ messages: prependUserContext(messagesForQuery, userContext), ... })` — **全量消息进入每次 API 调用**。
- 最终 body 由 `paramsFromContext` 组装（`<repo>/src/services/api/claude.ts:1538-1729`），关键字段：
  - `model`、`messages`（经 `addCacheBreakpoints`，claude.ts:1701-1709）、`system`（claude.ts:1710）、`tools`（全量工具 schema，claude.ts:1711）、`betas`（body 内数组，claude.ts:1713）、`metadata`（claude.ts:1714）、`max_tokens`（claude.ts:1715）、`thinking`（claude.ts:1716）、`temperature`（thinking 关闭时才发，claude.ts:1693-1695）、`context_management`（beta 门控，claude.ts:1718-1722）、`output_config`/`effort`、`speed` 等。
- 请求经 `anthropic.beta.messages.create({ ...params, stream: true }, ...)` 发出：`<repo>/src/services/api/claude.ts:1822-1832`。

### 1.2 messages 数组的形态与编排

- 序列化函数（决定发出去的 wire format）：
  - user → `userMessageToMessageParam`（`<repo>/src/services/api/claude.ts:588-631`）：`{role:'user', content: string | block[]}`；启用缓存时给**最后一块** content 加 `cache_control`。
  - assistant → `assistantMessageToMessageParam`（`<repo>/src/services/api/claude.ts:633-674`）：`{role:'assistant', content: block[]}`；`thinking`/`redacted_thinking` 块**不**参与 cache_control 标记（claude.ts:658-665）。
- 消息事件类型（块级）：
  - assistant 消息 content 由流式事件重建：`text`、`thinking`（带 `signature`，经 `signature_delta`）、`tool_use`（input 经 `input_json_delta` 累积，JSON 字符串拼接）、`server_tool_use`（advisor）：`<repo>/src/services/api/claude.ts:1995-2161`。
  - user 消息 content 含 `tool_result` 块（`tool_use_id` 关联）：`<repo>/src/services/tools/toolExecution.ts:396-408`（`{type:'tool_result', content, is_error, tool_use_id}`，消息带 `sourceToolAssistantUUID`）。
- 发送前 `normalizeMessagesForAPI`（`<repo>/src/utils/messages.ts:1989-2206`）：
  - 过滤 `progress`、`system`（本地命令除外）、合成 API 错误消息（messages.ts:2057-2075）；
  - **连续 user 消息合并**成一条（messages.ts:2094-2097）；
  - `attachment` 上浮到最近的 tool_result/assistant 之前（messages.ts:1996-1999）。

### 1.3 system prompt：每个请求都发，且每次重新组装

- `system` 字段在每次请求 body 中都存在：`<repo>/src/services/api/claude.ts:1710`（`system` 来自 `buildSystemPromptBlocks`，claude.ts:1376）。
- 每次请求都会在头部追加**当次计算**的块：attribution header（`x-anthropic-billing-header: cc_version=<version>.<fingerprint>; cc_entrypoint=<entrypoint>;...`，`<repo>/src/constants/system.ts:73-95`）、CLI 前缀（`You are Claude Code, Anthropic's official CLI for Claude.`，system.ts:10-18/30-46）、advisor/Chrome 指令等：`<repo>/src/services/api/claude.ts:1358-1369`。
- 其余大段（memory、MCP 指令、环境信息等）为**按会话缓存的 section**，`/clear`、`/compact` 时才重算：`<repo>/src/constants/systemPromptSections.ts:20-58`（`systemPromptSection` 缓存至 clear/compact；`DANGEROUS_uncachedSystemPromptSection` 每轮重算并打破缓存）。
- **量级证据**：注释称 header 切换会打爆 "~50-70K tokens" 的缓存前缀（`<repo>/src/services/api/claude.ts:1407-1408`）；dump-prompts 注释称 "system prompt + tool schemas = MBs"（`<repo>/src/services/api/dumpPrompts.ts:164`）。即 system+tools 恒定重负载，messages 才是增长主体。
- system 在缓存视角分块：`splitSysPromptPrefix`（`<repo>/src/utils/api.ts:321-435`）把 system 拆成 `[attribution(null), 前缀(org/global), 其余(org/global)]`，仅 1P global-cache 模式用 `SYSTEM_PROMPT_DYNAMIC_BOUNDARY` 分 static/dynamic。
- 另外每次请求把 `userContext`（CLAUDE.md 内容 + 当天日期，`<repo>/src/context.ts:155-189`，按会话 memoize）以 `<system-reminder>` 形式**前插**成一条 user 消息（`<repo>/src/utils/api.ts:449-474`，调用点 `<repo>/src/query.ts:660`）。注意这是**数组头部**的注入，不是尾部。

### 1.4 单轮 agent loop 的增长方式：全量回放 + 尾部追加

- 每轮迭代把**全部**（post-boundary）消息发给 API（见 1.1）。迭代结束时新内容拼在尾部进入下一轮：
  - `<repo>/src/query.ts:1715-1717` — `const next: State = { messages: [...messagesForQuery, ...assistantMessages, ...toolResults], ... }`
  - 即每轮 = `历史 + 本轮 assistant(含 thinking/tool_use) + 本轮 tool_result 们`。
- 流式响应每结束一个 content block 就产出一条 assistant 消息（一条消息一个块）：`<repo>/src/services/api/claude.ts:2171-2211`（`content_block_stop` → `{message: {..., content:[该块]}, requestId, uuid: randomUUID(), type:'assistant'}`）。
- 下一轮请求中这些 assistant 消息整体原样回放（仅 `normalizeMessagesForAPI` 过滤/合并）。

### 1.5 compact / truncate 机制源码命名

| 机制 | 源码位置 | 行为 |
|---|---|---|
| auto-compact（全量摘要） | `compactConversation` `<repo>/src/services/compact/compact.ts:387-` ；触发阈值 `getAutoCompactThreshold` `<repo>/src/services/compact/autoCompact.ts:72-91`（context window − 20K 摘要预留 − 13K buffer，autoCompact.ts:30/62） | 用 Haiku 请求生成摘要（`getCompactPrompt`，`<repo>/src/services/compact/prompt.ts:293-303`），然后数组替换为 `boundaryMarker + summary + attachments + hooks`（`buildPostCompactMessages` compact.ts:330-340，`CompactionResult` compact.ts:299-300） |
| compact 边界标记 | `createCompactBoundaryMessage` `<repo>/src/utils/messages.ts:4530-4555` | `{type:'system', subtype:'compact_boundary', compactMetadata:{trigger,preTokens,...}, logicalParentUuid}`；`getMessagesAfterCompactBoundary` `<repo>/src/utils/messages.ts:4643-4656` 从最后一个 boundary 切片（含 boundary），boundary 本身被 normalize 过滤 |
| microcompact / cache editing | `microcompactMessages` `<repo>/src/services/compact/microCompact.ts:253-293`；`addCacheBreakpoints` 的 `cache_edits` 机制 `<repo>/src/services/api/claude.ts:3108-3162` | 用 API 侧 cache 编辑删除旧 tool_result，本地消息不动；boundary 消息 `createMicrocompactBoundaryMessage` messages.ts:4557-4583 |
| snip（HISTORY_SNIP） | `snipCompactIfNeeded`（query.ts:401-410 调用） | 从模型可见视图剔除旧内容，UI 保留 |
| context collapse | `applyCollapsesIfNeeded`（`<repo>/src/query.ts:440-447`） | 把归档区间折叠成 `<collapsed>` 摘要占位符，持久化为 `marble-origami-commit` 条目（`<repo>/src/types/logs.ts:255-269`） |
| PTL 重试截头 | `truncateHeadForPTLRetry` `<repo>/src/services/compact/compact.ts:243-` | compact 请求自身 prompt-too-long 时从头部丢最老的 API 轮次组 |
| 媒体超限裁旧 | `stripExcessMediaItems` `<repo>/src/services/api/claude.ts:956-1015` | >100 媒体时删最老，保最新 |
| 每请求缓存点 | `addCacheBreakpoints` `<repo>/src/services/api/claude.ts:3063-3211` | `cache_control` 标记**只打到最后一条消息**（`markerIndex = messages.length - 1`，skipCacheWrite 时为倒数第二条，claude.ts:3089）；缓存前缀内的 tool_result 块加 `cache_reference` |

## 2. delta 位置：新内容永远在尾部

- 每轮请求中，相对上一轮请求的**全部新内容**（新 user 消息、新 assistant 块、新 tool_result）都位于 messages 数组**尾部**：
  - 追加点：`<repo>/src/query.ts:1715-1717`（下一轮 state.messages = 旧 + 新尾部）；
  - REPL UI 侧同样尾部追加：`<repo>/src/screens/REPL.tsx:2629`（`setMessages(oldMessages => [...oldMessages, newMessage])`）。
- 缓存标记打在最后一条消息上（`<repo>/src/services/api/claude.ts:3089`）——**请求数组的末尾 = 服务端缓存断点 = 最新证据所在**。
- 例外（新内容不在尾部）：
  - `userContext`（CLAUDE.md reminder）**前插在头部**（`<repo>/src/utils/api.ts:461-473`）；
  - tool search 的 deferred 工具列表可前插为 meta user 消息（`<repo>/src/services/api/claude.ts:1337-1344`）；
  - compact 后数组被替换为 `boundary + summary + 保留段 + 新尾部`（compact.ts:330-340），boundary 是新的"逻辑起点"。
- 因此 tail-first 截断策略的正确性成立：**截掉头部不丢任何未归档的新证据**；head 保留的需求仅来自缓存命中率（cache_control 前缀）而非证据完整性。

### 2.1 并行 tool_use 与结果顺序（邻接不可靠）

- 流式响应**每结束一个 content block 就产出一条 assistant 消息**（`<repo>/src/services/api/claude.ts:2171-2211`）——并行 `tool_use` 表现为多条 assistant 消息（每条一个块）。
- `tool_result` 块由本地工具执行收集进下一条 user 消息（`<repo>/src/services/tools/toolExecution.ts:396-408`），**wire 顺序与 tool_use 出现顺序不一定一致**；读取端必须 `recoverOrphanedParallelToolResults` 按 `message.id` 分组恢复兄弟 tool_use（`<repo>/src/utils/sessionStorage.ts:2118-2206`）——只能按 `tool_use_id` 配对，不能靠邻接/顺序。
- **"result 后必有 continuation"不成立**：tool 执行后 loop 通常立即再调 API（`<repo>/src/query.ts:1395-1400 → 1716`），tool_result 因此恒在下一次请求尾部可见；但若会话在最终回答后结束（或被中断），**该轮 tool_results 与最终 assistant 文本不会出现在任何被观察请求中**（只存在于本地 transcript）——捕获侧只见"下一次请求"，最后一轮的产出可能永久不可见。

## 3. 分支与 session 追踪

### 3.1 session 与 transcript 文件

- 会话 ID：进程启动时 `randomUUID()`（`<repo>/src/bootstrap/state.ts:331`），`getSessionId()` state.ts:431-433。
- 文件路径：`<~/.claude|CLAUDE_CONFIG_DIR>/projects/<sanitized-cwd>/<sessionId>.jsonl`：
  - `getProjectsDir()` `<repo>/src/utils/sessionStorage.ts:198-200`；`getTranscriptPath()` sessionStorage.ts:202-205；`getClaudeConfigHomeDir` = `CLAUDE_CONFIG_DIR ?? ~/.claude` `<repo>/src/utils/envUtils.ts:7-14`。
- 子代理 sidechain：`.../projects/<dir>/<sessionId>/subagents/agent-<agentId>.jsonl`（`<repo>/src/utils/sessionStorage.ts:247-258`）；另有 `.meta.json`（agentType/worktreePath，sessionStorage.ts:260-290）。
- 写入是**增量 tail 追加**：`useLogMessages` 只把新 tail `slice` 交给 `recordTranscript`（`<repo>/src/hooks/useLogMessages.ts:22-25, 59-64`）；`insertMessageChain` 逐条写并维护 `parentUuid` 链（`<repo>/src/utils/sessionStorage.ts:993-1083`）。
- JSONL 每行一个 `Entry`（`<repo>/src/types/logs.ts:297-318`），含消息类与元数据类（summary/custom-title/last-prompt/task-summary/content-replacement/mode/worktree-state 等）。
- 文件可增长到 GB 级：`MAX_TRANSCRIPT_READ_BYTES = 50MB` 读取上限（sessionStorage.ts:227-229）；读取端对大文件**跳过 compact boundary 之前的内容**（`SKIP_PRECOMPACT_THRESHOLD`、`readTranscriptForLoad`、`walkChainBeforeParse`，sessionStorage.ts:3536-3579）。

### 3.2 每消息的追踪字段（TranscriptMessage）

`<repo>/src/types/logs.ts:8-17`（SerializedMessage）与 `logs.ts:221-231`（TranscriptMessage）：

- `cwd`、`userType`、`entrypoint`、`sessionId`、`timestamp`、`version`、`gitBranch?`、`slug?`（每消息都盖章，写入点 sessionStorage.ts:1057-1063）；
- **`parentUuid: UUID | null`** — 链式父指针（sessionStorage.ts:1040；tool_result 用 `sourceToolAssistantUUID` 覆盖，sessionStorage.ts:1028-1037）；
- **`logicalParentUuid?`** — "parentUuid 被置空（session break / compact boundary）时保留逻辑父"（sessionStorage.ts:1041；类型注释 logs.ts:223）；
- **`isSidechain: boolean`** — 子代理侧链标记（sessionStorage.ts:1042）；**`agentId?`**（侧链 resume 用，logs.ts:226）；
- `promptId?`（OTel prompt.id 关联，sessionStorage.ts:1045-1046）。
- /branch 额外写 **`forkedFrom: {sessionId, messageUuid}`**（`<repo>/src/commands/branch/branch.ts:26-31, 124-133`）。

### 3.3 分支机制

- **/branch**（`<repo>/src/commands/branch/branch.ts:61-173`）：复制当前 transcript 到**新 sessionId** 的 jsonl，逐条改写 `sessionId`、`parentUuid`（重建链）、加 `forkedFrom`，标题后缀 " (Branch)"（branch.ts:179-220）。是全量拷贝，不是 delta。
- **/rewind**（`<repo>/src/screens/REPL.tsx:3661-3707`）：`setMessages(prev.slice(0, messageIndex))` — 在**同一 session** 内把数组截到选中的 user 消息，之后新消息继续在原文链上长出来；UI 侧 `setConversationId(randomUUID())` 只为重挂组件（REPL.tsx:3673）。
- **/clear**：`regenerateSessionId({setCurrentAsParent:true})`（`<repo>/src/commands/clear/conversation.ts:201-208`；`<repo>/src/bootstrap/state.ts:435-450`），旧 sessionId 记入 `parentSessionId`（state.ts:452-454），并 `resetSessionFilePointer()`（新文件惰性创建，sessionStorage.ts:1501-1507）。
- **--resume / --fork-session**：
  - 恢复 = `loadTranscriptFile`（`<repo>/src/utils/sessionStorage.ts:3472-`）→ 找 **leafUuids**（无人指向的 parentUuid = 链尖端）→ 取最新非 sidechain leaf → `buildConversationChain`（sessionStorage.ts:2069-2094，沿 parentUuid 回溯）→ `removeExtraFields`（**剥掉 parentUuid/isSidechain**，sessionStorage.ts:1814-1821）。
  - 默认复用原 sessionId（`<repo>/src/cli/print.ts:5147-5156`）；`--fork-session` 保持新 sessionId（sessionRestore.ts:435-471）；`--resume-session-at <uuid>` 按消息 uuid **切片**恢复（print.ts:5106-5120）。
  - 并行 tool_use 的 DAG 链在读取端恢复（`recoverOrphanedParallelToolResults` sessionStorage.ts:2118-2206，按 `message.id` 分组兄弟 assistant）。
- **/resume 列表排序** = 文件 mtime（`sortLogs` `<repo>/src/types/logs.ts:319-330`；`LogOption.modified` logs.ts:24-25）。

## 4. UA 与请求头识别

- 主 API 客户端默认头（`<repo>/src/services/api/client.ts:105-116`）：
  - `'x-app': 'cli'`
  - `'User-Agent': getUserAgent()`
  - `'X-Claude-Code-Session-Id': getSessionId()`
  - 可选：`x-claude-remote-container-id`（`CLAUDE_CODE_CONTAINER_ID`）、`x-claude-remote-session-id`（`CLAUDE_CODE_REMOTE_SESSION_ID`）、`x-client-app`（`CLAUDE_AGENT_SDK_CLIENT_APP`）
  - 额外保护头 `x-anthropic-additional-protection`（client.ts:124-129）
- UA 精确模板（`<repo>/src/utils/http.ts:18-35`）：
  - `claude-cli/${MACRO.VERSION} (${process.env.USER_TYPE}, ${process.env.CLAUDE_CODE_ENTRYPOINT ?? 'cli'}${', agent-sdk/<ver>'?}${', client-app/<app>'?}${', workload/<w>'?})`
  - 注释明确：日志过滤依赖 `claude-cli` 字样，勿改（http.ts:16-17）。
- 简版 UA `claude-code/${MACRO.VERSION}`（`<repo>/src/utils/userAgent.ts:8-10`），用于 MCP 之外的服务（settingsSync、teamMemorySync、usage 等）。
- 认证头：订阅用户 `Authorization: Bearer <oauth>`（http.ts:70-83）；API key 用户 `x-api-key`（http.ts:87-98）。
- 附加头：`x-client-request-id`（1P 专用，`<repo>/src/services/api/client.ts:356`，写入点 claude.ts:1813-1829）；SDK 自带 `anthropic-version` 等。
- body 内识别字段：`metadata.user_id` = JSON 字符串 `{device_id, account_uuid, session_id}`（`<repo>/src/services/api/claude.ts:503-528`）；body 内 `betas: [...]` 数组（claude.ts:1713）；attribution 头在 **system 首块**：`x-anthropic-billing-header: cc_version=<VERSION>.<fingerprint>; cc_entrypoint=<entrypoint>;`（`<repo>/src/constants/system.ts:73-95`，fingerprint 由首条 user 消息计算，claude.ts:1322-1325）。

## 5. 输出捕获机会（只抓请求时的损失评估）

- **响应流里没有 tool 输出**。响应流事件只有 `message_start / content_block_start / content_block_delta(text|thinking|signature|input_json) / content_block_stop / message_delta(usage) / message_stop`（`<repo>/src/services/api/claude.ts:1979-2220`）。tool 执行输出（stdout、文件 diff 等）由本地工具执行产生，作为 `tool_result` 块包装在 **user 消息**里（`<repo>/src/services/tools/toolExecution.ts:396-408`），**只在下一次请求的 messages 尾部**出现（query.ts:1395-1400 → 1716）。
- 因此：只捕获请求（input）的审计层，能看到：
  - 每次请求的**完整历史**（全量回放）→ 上一轮的 tool_result、thinking、assistant 文本都在本次请求里，只是"迟到一轮"；
  - 而**本次响应新增的文本/思考/tool_use** 要到**下一次请求**才可见。
- 响应流里独有的、请求里拿不到的字段：
  - 每个 assistant 消息的 `message.id`（API 消息 ID）与 `requestId`（`<repo>/src/services/api/claude.ts:2192-2208`；`streamRequestId = result.request_id` claude.ts:1834）；
  - `usage`（input/output/cache_read/cache_creation tokens，`message_delta` 累积，claude.ts:2213-2214；`updateUsage` claude.ts:2924）；
  - `stop_reason`（claude.ts:1767）与 stream 时序（TTFT 等）。
  - 注：assistant 消息的 `requestId` 会随下一轮请求的 messages 回放，所以"上一条 assistant 的 request_id"仍可恢复；但 usage/stop_reason 不可恢复。
- 客户端自带的"增量归档"参考实现：`createDumpPromptsFetch`（`<repo>/src/services/api/dumpPrompts.ts:146-226`）——init 一次写 `system/tools/metadata`，变化时写 `system_update`，之后**每请求只写新增 user 消息**，响应另存 SSE chunks。其指纹法（model|toolNames|sysLen，dumpPrompts.ts:74-88）可直接借鉴为 delta 检测。
- 官方 bug-report 的请求缓存只保留最近 5 条（dumpPrompts.ts:14-15）。

## 6. 对 tail-first 截断策略有直接影响的 5 条事实

1. **全量回放使"最新证据"永远在数组尾部**：agent loop 每轮请求携带 post-boundary 全部消息（`<repo>/src/query.ts:365, 660`），下一轮 = `旧 + assistant + tool_results` 尾部拼接（`<repo>/src/query.ts:1715-1717`）。tail-first 保留 = 保留每一轮的最新增量链，无信息损失窗口。
2. **尾部就是缓存断点**：`cache_control` 只打在最后一条消息（`<repo>/src/services/api/claude.ts:3089`），microcompact 的 `cache_edits` 也插在最后一条 user 消息（claude.ts:3142-3161）——截断策略若保留尾部，与官方缓存语义天然对齐；截头只影响缓存命中，不影响内容完整。
3. **头部的恒定负载可安全丢弃**：每次请求头部是 attribution header + CLI 前缀 + 缓存的 system sections + 前插的 `<system-reminder>` CLAUDE.md（`<repo>/src/services/api/claude.ts:1358-1369`，`<repo>/src/utils/api.ts:449-474`）；同会话内逐请求近似不变，量级 "~50-70K tokens"（claude.ts:1407-1408），可作为去重/丢弃对象。
4. **唯一需要跨请求对齐的键是 tool_use_id**：tool_result 靠 `tool_use_id` 配对（toolExecution.ts:396-408；缓存侧 `cache_reference: tool_use_id` claude.ts:3201-3203），assistant 的 tool_use 与下一次请求的 tool_result 跨请求分离——结构化存储若按"请求"分桶，需在消息级以 tool_use_id 关联；按"会话"聚合则无此问题（回放里两者都在）。
5. **compact 边界 = 逻辑重载点，保留尾部即可无损跳过**：compact 把历史压缩成 `compact_boundary + summary` 后从边界继续（`<repo>/src/services/compact/compact.ts:598-624`、`<repo>/src/utils/messages.ts:4530-4555`），读取端对大文件只扫 boundary 之后（sessionStorage.ts:3536-3579）——observer 可以同样"遇到 compact_boundary 即归档旧段"；而 /rewind（REPL.tsx:3661-3671）在同一 session 内截短数组，尾部保留策略自然跟随最新链。

---

## 附：主要证据文件索引

| 主题 | 文件 |
|---|---|
| 请求组装 | `<repo>/src/services/api/claude.ts`（588-674 序列化、1358-1369 system 头部、1538-1729 params、1699-1728 body、1822-1832 发送、1979-2220 流解析、3063-3211 缓存点） |
| agent loop | `<repo>/src/query.ts`（365 全量复制、449-451 system 组装、659-708 callModel、1395-1400/1716 tool_results 尾部、1062/1715-1728 迭代续接） |
| REPL 入口 | `<repo>/src/screens/REPL.tsx`（2629 追加、2793-2801 query 调用、3661-3707 rewind） |
| compact | `<repo>/src/services/compact/compact.ts`、`autoCompact.ts`、`microCompact.ts`、`prompt.ts`；`<repo>/src/utils/messages.ts:4530-4656` |
| transcript 存储 | `<repo>/src/utils/sessionStorage.ts`（198-205 路径、993-1083 写入链、1451-1462 sidechain、1814-1821 字段剥离、2069-2206 链重建、3472+ 读取） |
| 类型 | `<repo>/src/types/logs.ts`（8-17/221-231 追踪字段、297-318 Entry） |
| session 状态 | `<repo>/src/bootstrap/state.ts`（331 sessionId、435-478 切换/再生） |
| 分支 | `<repo>/src/commands/branch/branch.ts`；`<repo>/src/cli/print.ts:5106-5120`（resumeSessionAt） |
| 头与 UA | `<repo>/src/services/api/client.ts:105-129/356`、`<repo>/src/utils/http.ts:18-98`、`<repo>/src/utils/userAgent.ts`、`<repo>/src/constants/system.ts:73-95` |
| 增量归档参考 | `<repo>/src/services/api/dumpPrompts.ts` |
| 子代理 | `<repo>/src/tools/AgentTool/runAgent.ts:732-805`（sidechain 录制）、sessionStorage.ts:247-258（路径） |
