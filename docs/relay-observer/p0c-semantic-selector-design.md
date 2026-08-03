# P0-C 设计：语义选择器（semantic selector）—— 稳定 anchor + 单个明确 gap + 最新完整语义单元

日期：2026-08-03（外部审核裁决后修订；旧文件名 `p0c-tail-biased-selection-design.md`，已更名为本文档）
状态：设计定稿（修订版），待实现
依据：`research/codex-session-report.md` + `research/claude-code-session-report.md`（源码事实）+ `reservation-budget-findings.md`（实弹数据）+ P0-B 已实现的预算拆解 + 外部审核裁决（2026-08-03）

## 0. 核心目标（正式确定）

```
完整请求能放下 → 原样完整保留（零 gap）
完整请求放不下 → 稳定的少量 anchor + 单个明确 gap + 最新完整语义单元
```

- 完整放得下：不做任何选择，`total <= limit` 时零 gap、零省略。
- 完整放不下：**头部少量稳定 anchor**（不依赖 items[0] 位置，见 §6 不变量 3）+ **单个 position 明确的 gap** + **尾部最新完整语义单元**（按语义单元倒序选择，tool 配对不可拆）。
- "语义单元"定义：以证据语义（消息、工具交换、compact 摘要）为粒度的最小原子选择单位，见 §3 阶段二。

## 1. 五条研究结论修正（相对旧版 tail-biased 设计的表述）

### 1.1 system/developer 需要有限 anchor budget（不依赖 content dedup）

**旧表述错误**："system/developer 有 dedup 所以不需要特殊处理"。

**事实**：content-object 按 digest 去重发生在 selector **之后**（`insertContentObjectsTx`，`persistence.go:494`，`ON CONFLICT (session_id, item_digest) DO NOTHING`）。重复的 21KB system/developer 内容虽然不重复保存 payload，但：
- 每个 turn 的 canonical 序列里它仍然**原样出现**（select 的是 digest 列表，不是去重后的存储）；
- 仍消耗当前 turn 的 capture budget、出现在 digest list、影响 context 大小；
- 截断场景下挤占**最新证据的预算空间**（claude system ~50-70K tokens、codex instructions 21KB，见研究报告）。

**正确结论**：存储层按 digest 去重 ✓（保持现状）；但**选择层仍需有限 anchor budget**——防止巨大稳定前缀挤掉最新证据。首版实现：system/developer 作为候选 anchor（§6 不变量 3），受**锚预算上限**约束（总 anchor 预算 = capture limit 的固定比例，首版建议 ≤ 1/4，实现 PR 以 corpus benchmark 校准）。

### 1.2 `x-codex-window-id` 变化 = compaction 窗口边界，不是新 session

**旧表述**：把 window 边界当作归档分段事件处理（概念接近，但领域关系不清）。

**事实与裁决**：
- 领域关系是 **session/thread 下分 window A/B**（`window_id = "{thread_id}:{window_number}"`，`core/src/session/mod.rs:3474-3479`）；每个 window 含多个 turn。window 变化 ≠ 新 session。
- **本轮不引入 schema v4，不为 window 重构 DB/session key**。只需三件事：
  1. 报告固化 window 语义（window = session 内分段，见研究报告 §2.2 / §3.3）；
  2. selector **不把 summary 当原始历史**（compact_summary 单元语义标记，见 §3 阶段二 kinds；重建时 summary 是锚/摘要，不是可展开的原文）；
  3. 为未来 `AgentLoopWindow` 模型留接口（canonical item 的 kind 字段与 digest 列表已可表达，不新增列）。
- window 分段、fork DAG、subagent 关系**单独列入后续 AgentLoop 产品线**，不在 P0-C 实现。

### 1.3 区分两类缺失：`source_truncated` vs `capture_truncated`

**事实**：
- `source_truncated`：**客户端**已压缩/截断的证据——codex `truncate_middle` 保头保尾裁中段（`utils/string/src/truncate.rs:38-68`，观测到的是头尾两段 + `Warning: truncated output` 前缀）；claude compact 后的 summary/boundary；auto-compact 替换掉的旧轮次。
- `capture_truncated`：**Observer 自己**因预算省略的证据（本 selector 的 gap）。

**正确结论**：不能把 Codex 已裁过的工具结果当完整原文；两类缺失分别标注（GapInfo 的 `reason` 与 `source_truncated` 字段，§4）。source_truncated 的"已省略字节"由客户端报告（如 truncated output 前缀里的 original token count）或不可得；**不编造**客户端未提供的省略字节（§4 约束）。

### 1.4 元数据是结构化客户端身份声明，不是权威身份

**旧表述**：研究报告列了 `x-codex-turn-metadata` / `X-Claude-Code-Session-Id` 等完整标识链，可"零解析做关联"。

**裁决**：`x-codex-turn-metadata` 等是**结构化客户端身份声明**（客户端自报，含 sandbox/workspace 等可被伪造的字段），不是可信的权威身份。当前 `node_scope + user_id + HMAC alias` 隔离方向正确（alias digest = 声明内容的 HMAC，绑定到本节点用户维度）；**不因 metadata 丰富就用裸 thread_id/session_id 做全局主键**。首版沿用现有 alias 机制，metadata 字段只作为 alias 的输入之一，不提升为信任边界。

### 1.5 工具输出头尾拼接保持为一个原子 unit（按稳定 ID 匹配，不靠邻接）

**旧表述**：v1 简化"tool_result 与其 tool_call 视为一个选择单元"，未明确配对键与邻接问题。

**裁决**：工具输出的**头段 + 中段 gap + 尾段**（codex truncate_middle 的观测形态）作为一个原子 unit 处理：

```
ToolExchangeUnit {
    call,                          // codex function_call / claude tool_use
    result: {
        preserved_head,            // 客户端保留下来的头部段
        source_gap,                // 客户端裁掉的中段（source_tool_truncation，字节数仅当客户端报告）
        preserved_tail,            // 客户端保留下来的尾部段
    }
}
```

- 按**稳定 ID 匹配**：codex `call_id`（function_call ↔ function_call_output）/ claude `tool_use_id`（tool_use ↔ tool_result，`toolExecution.ts:396-408`）；
- **不依赖邻接**：并行调用/异步结果会破坏邻接（codex `drain_in_flight` 按完成顺序追加，`turn.rs:1908-1928`；claude 并行 tool_use 的读取端要 `recoverOrphanedParallelToolResults`，`sessionStorage.ts:2118-2206`）；
- 头尾拼接语义进存储模型：`preserved_head` + `source_gap` + `preserved_tail` 按 ID 重建，绝不把尾段当独立完整输出。

## 2. 源码事实（交叉确认，与研究报告一致）

| 事实 | codex 证据 | claude 证据 |
|------|-----------|------------|
| 全量回放 | `core/src/session/turn.rs:273-279` 每步整体重建 input | `src/query.ts:365,660` 全量 messages 进每次调用 |
| 新内容恒在尾部 | `context_manager/history.rs:126-135` 只追加尾部 | `query.ts:1715-1717` 旧+assistant+tool_results 尾部拼接 |
| 自身回收也是保尾 | `compact.rs:285-294` 删头保尾；auto-compact 尾部 20k token + 摘要 | auto-compact 数组替换为 boundary+summary；缓存点 `cache_control` 只打最后一条（`claude.ts:3089`） |
| 工具输出截断 | 保头+保尾中段裁（`utils/string/src/truncate.rs:38-68`） | 本地执行后包装为 tool_result，只在下一次请求尾部出现 |
| tool 配对键 | `call_id`（function_call ↔ function_call_output，`history.rs:355-368` 归一化补全/剥离孤儿） | `tool_use_id`（tool_use ↔ tool_result，`toolExecution.ts:396-408`；读取端 `recoverOrphanedParallelToolResults`） |
| compact 标识 | `compaction`/`context_compaction` item + `x-codex-window-id` 变化 | `compact_boundary` system 消息 + `logicalParentUuid` |
| 系统提示词 | `instructions` 字段 21KB + developer message 首轮全量后续 diff | system 字段每请求重发 ~50-70K tokens，同会话近似不变 |
| 分支 | fork/rollback/rewind 一等公民（`InitialHistory::Forked`、`ThreadRolledBack`） | /branch 全量拷贝+forkedFrom、/rewind 同 session 截短、`parentUuid` 链 |
| 完整标识链 | `x-codex-turn-metadata`（installation/session/thread/turn/window/request_kind/forked_from/parent_thread/subagent_kind/sandbox/workspace） | `X-Claude-Code-Session-Id` + `metadata.user_id` JSON + `parentUuid`/`isSidechain`/`logicalParentUuid` |
| 响应侧独有（请求捕获缺失） | 未截断原始工具输出、推理明文（加密）、token usage、流式时序 | usage、stop_reason、message.id（requestId 可随下轮回放恢复） |

## 3. 三阶段实现结构（替代旧版"在 finishNormalize 中实现"的单层描述）

选择逻辑拆为三个可独立测试的阶段，前两个阶段是纯函数，不接触 DB 与预算。

### 阶段一 协议规范化（现有 normalizer，不动行为）

白名单 / HMAC / logical bytes / 协议差异（codex `responses` vs claude `messages`）归一为 `CanonicalItem`。**不做预算选择**。现状保持。

### 阶段二 语义分组（新纯函数层）

```go
func BuildSemanticUnits(items []CanonicalItem) []SemanticUnit

type SemanticUnit struct {
    Kind            SemanticUnitKind
    Items           []CanonicalItem
    LogicalBytes    int64
    CanonicalBytes  int64
    CallIDs         []string
    Anchor          bool
    SourceTruncated bool
}
```

首版 kinds：`anchor` / `message` / `tool_exchange` / `compact_summary` / `unknown`。

硬规则：
- **tool call + result 原子化**（`tool_exchange`）：call 与 result 配对进同一 unit，按稳定 ID（codex `call_id` / claude `tool_use_id`），不靠邻接；
- **多个并行 calls 按 ID 匹配**；无法匹配的孤立 result 仍保留但标记 `orphan`（unit 的 `CallIDs` 非空、pair 状态记录于内部字段，不丢弃证据）；
- unit 内顺序保持协议原顺序；
- 构建结果必须**确定性**（同输入恒同输出——决定性的分组与排序，供测试与 corpus benchmark 复用）。

### 阶段三 预算选择（新纯函数层）

```go
func SelectEvidence(units []SemanticUnit, limit int64) SelectionResult
```

算法顺序：
1. **精确测量**完整 canonical 总量（unit.CanonicalBytes 求和）；
2. `total <= limit` → **完整返回，零 gap**（核心目标第一分支）；
3. `total > limit`：
   a. 构造候选 anchor（§6 不变量 3 的候选集与优先级，受锚预算上限约束）；
   b. **尾部倒序**选择完整 semantic units（unit 原子：要么整单元、要么进 gap；`tool_exchange` 绝不拆 call/result）；
   c. 计算被省略区间（anchor + 已选尾部之外的 items 区间）；
   d. 构造实际 gap（§4，`reason=capture_budget`，position 由区间位置定）；
   e. **用 gap 真实序列化大小重新校验**（gap marker 本身进预算；若超限则收缩 anchor 或尾部，直到 gap 放得下；最坏情形走 §5 超大单元路径）。

**明确写：不用固定 envelope 先减预算**（P0-B 的 `selectionBudget = limit - envelope` 只作 P0-B 阶段的最小 marker 兜底口径；P0-C 的校验以真实序列化大小回环，不预先扣一个固定常数）。

## 4. Gap 结构（首版扩展 canonical JSON，不迁移 DB）

```go
type GapInfo struct {
    Position        string `json:"position"`           // head | middle | tail
    Reason          string `json:"reason"`
    OmittedItems    int    `json:"omitted_items"`
    LogicalBytes    int64  `json:"logical_bytes"`
    SourceTruncated bool   `json:"source_truncated,omitempty"`
}
```

`reason` 枚举：
- `capture_budget` —— Observer 因预算省略（选择器主动裁）；
- `source_compaction` —— 客户端压缩（compact 边界/窗口变化），只描述观察到的窗口变化或 summary，**不编造 omitted byte count**（LogicalBytes 可为 0 或省略）；
- `source_tool_truncation` —— 客户端工具输出截断（codex truncate_middle 中段）；
- `item_count_limit` —— 条目数上限（如未来引入时）；
- `oversized_semantic_unit` —— 最新超大语义单元（§5）。

约束：**不伪造客户端未提供的省略字节**。`source_truncated=true` 且客户端报告了 original token count 时才填 `logical_bytes`；否则只标注类型。

## 5. 最新超大语义单元处理

当最新（尾部）语义单元超过剩余预算时，**禁止**：
- 拆 call/result（破坏 §6 不变量 2）；
- 只留 result 尾部丢 call（丢失调用语义）；
- 返回零 items（与旧版短路同源的问题，见 `reservation-budget-findings.md` §2.4）；
- 让 session tracking 失效。

**结果 = 专用的 oversized-unit gap**（`reason=oversized_semantic_unit`），保留：
- unit 类型（Kind）；
- call ID 的 HMAC 或稳定 ID（可关联、可验证）；
- logical bytes；
- 是否 source-truncated。

**session/content 解耦合同继续生效**：即使 selector 零正文 item，会话仍被跟踪（身份解析与内容选择解耦，见 `reservation-budget-findings.md` §3.3 短路保护——gap 状态且 items 空时仍应尝试身份解析）。

## 6. 七条硬不变量

1. **full-fit 优先**：`total <= limit` 时原样完整保留，零 gap。
2. **tool 单元原子化**：call/result 同一选择单位，**按 ID 匹配（codex call_id / claude tool_use_id）不靠邻接**；孤儿 result 保留并标记，绝不拆对。
3. **anchor 不是固定 items[0]**。候选集：system/developer 指令、compact summary、最新用户指令、agent 任务/计划状态（codex world-state/contextual user message 增量、claude CLAUDE.md userContext/memory sections）。首版建议优先级（依据两份源码报告）：
   1. 最新用户指令 + 其工具链（尾部选择主体，接受标准所系）；
   2. compact summary（窗口重载后的新基线；claude 为 Haiku 生成摘要、codex 为最后 assistant 全文——作 anchor 保留但标记 `compact_summary`，不当作原始历史）；
   3. system/developer 指令（稳定锚，受锚预算上限约束；claude ~50-70K tokens 超限时放弃全量，依赖 digest dedup 跨请求恢复）；
   4. agent 任务/计划状态。
   锚预算上限：首版 = capture limit 的固定比例（建议 ≤ 1/4），corpus benchmark 校准；"稳定少量 anchor" 而非"保全部头部"。
4. **tail 从语义单元倒序选择**：以 unit 为粒度从尾部向前，直到预算耗尽。
5. **gap 唯一且位置明确**：每个输出最多一个 gap，`Position`（head | middle | tail）确定；anchor 在头、最新证据在尾时 gap 落在中间区间。
6. **session tracking 永远独立**：selector 零 items / 超大 unit / 协议不支持 / panic 均不影响身份绑定（session 解析在内容选择之外，独立路径保证）。
7. **首版不改 schema**：输出普通 ordered canonical items（+ gap marker item），不做 prefix+suffix 双端 delta、不引入 schema v4、不新增 context 类型。**实现 PR 必须额外测 context storage amplification**——anchor + gap + tail 布局可能降低公共前缀命中（gap marker 内容随 omitted 变化，commonPrefix 在 marker 处断裂），须以 `suffix bytes / full bytes` 量化并记录基线。

## 7. 锁顺序事实（T3，修正文档描述）

**实际代码顺序（append path，`pkg/relay_observer/persistence.go`）**：

```
session → content objects → head
```

1. `resolveSessionTx` → `lookupAliasSessionTx(..., lockSession=true)`：`FOR UPDATE OF s` 锁 **session row**（`persistence.go:456`）——**session row 是统一首锁**（与 retention 删除路径共享的串行化边界）；
2. `insertContentObjectsTx`（`persistence.go:308`，定义 `:494`）：插入 **content objects**（`ON CONFLICT (session_id, item_digest) DO NOTHING`，携带对冲突行的隐式锁）；
3. `lockHeadTx`（`persistence.go:314`，定义 `:588`）：`FOR UPDATE` 锁 **head row**。

即 `insertContentObjectsTx` 在 `lockHeadTx` **之前**执行。retention 路径持有 `session → head`，从不持有 content 行锁的逆序。全系统不存在 `head → session` 或 `head → content` 反序路径，因此**不会产生之前担心的 session/head 死锁**——但注释与测试说明必须按此实际顺序书写。

注意：`persistence.go:445` 注释仍写 "append always holds session → head → content"，与代码顺序不符（本 PR 纯文档，不动 .go）；**实现 PR 应顺带修正该注释及 retention 测试中的锁序说明**。

## 8. 权衡与风险

### 8.1 context suffix 放大 / 公共前缀命中下降

anchor + gap + tail 布局下，gap marker 内容随每轮 omitted 变化 → commonPrefix 在 marker 处断裂 → delta suffix 接近全量 → context 元数据放大（GPT 方案 A 已接受，`reservation-budget-findings.md` 同源）。v1 接受，以 corpus benchmark 量化：
- capture recall（最新消息保住率，接受标准）；
- context storage amplification（suffix bytes / full bytes 比，**实现 PR 必测**，不变量 7）；
- content-object dedup ratio（跨 turn 复用率）；
- common_prefix_count 分布。
方案 B（prefix+suffix 双端 delta）记为 v2 候选，不在 v1 实现。

### 8.2 配对单元大小

`tool_exchange` 单元超预算（工具输出大）时：不拆对；整单元进 gap 则按 §5 处理（最新单元）或普通 capture_budget gap（中部单元），并标注 `source_truncated` 保留客户端截断信息。

### 8.3 已知局限（请求侧捕获固有）

- 原始工具输出中段不可恢复（codex 保头+保尾截断；claude tool_result 在下次请求尾部）；
- usage/stop_reason/推理明文只响应侧有 → 如需须响应侧旁路（v2 评估，领域接口预留：阶段二 unit 结构与响应侧证据可共存，不阻塞）。

## 9. 验收

```bash
# 单元：语义分组 / 配对保护（按 ID 不靠邻接）/ tail 单元选择 / gap position / oversized unit / 预算回环校验
go test ./pkg/relay_observer/ -run 'TestBuildSemanticUnits|TestSelectEvidence|TestGapPosition|TestOversizedUnit' -count=1

# corpus benchmark（固定 corpus：4 个 agent fixture + 合成滑动窗口；含 amplification 基线）
go test ./pkg/relay_observer/ -bench 'BenchmarkSemanticSelector' -benchtime=200x

# 集成（真实 PG）：tail 截断 + 重建 + 配对标注 + 死链检查
go test -tags relay_observer_pg_integration ./pkg/relay_observer/ -run TestIntegrationSemanticSelector -count=1
```

**接受标准**：
- 最新 user 消息 + 其 tool 链 100% 保住（预算 ≥ 单元大小）；
- gap marker 恒存在（任何截断输出都带 §4 结构）；
- 重建无死链（配对被省略时孤儿标记明确，不产生悬空引用）。

## 10. 后续（不属本轮）

- AgentLoop 产品线：window 分段、fork DAG、subagent 关系、`AgentLoopWindow` 模型；
- output-side evidence 旁路（v2 评估）；
- prefix+suffix 双端 delta（方案 B）；
- `persistence.go:445` 锁序注释修正（随实现 PR）。
