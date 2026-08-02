# 会话审计报告：Relay Observability 收尾 · 仓库重组 · CI 迁移

日期：2026-08-02 ~ 2026-08-03
范围：本会话全部操作记录（供外部审核复核；不含任何密钥/口令/token 明文）

## 0. 一句话摘要

Relay Observability 产品线（审计线）已完整交付并上线测试站；随后完成仓库拓扑重组（org → 个人私有）、CI 迁移到 sgp2 自托管 runner（双实例并行）、以及证据层优化的调研启动。当前唯一进行中的工作线是证据层优化（任务 #11）。

## 1. 背景与目标链

1. Relay Observability 产品线（T1.1 → T5.4）交付：审计线两阶段写入、Root 查询 API、原生设计 token 的 Root UI、有界索引化 retention、fail-open 保障。
2. 公开可访问的测试站（用户要"自己去看看前端"）。
3. 站点加速：CF Tunnel 在 sgp2 出站受限（QUIC/7844 被阻）→ 弃用，改 nginx 反代 + CF 橙云 DNS + UFW 开放 80/443。
4. 概览页（overview）从"最近 1 个窗口"重构为 KPI 卡 + VChart 连续窗口趋势（严格按原生 NewAPI 设计哲学：StatCard / VChart / useChartTheme / i18n key=English）。
5. 仓库拓扑重组（用户最终拍板："设置为个人仓库，private。组织的那俩 newapi 删掉。本地改名 newapi"）。
6. CI 迁移到 sgp2 自托管 runner（个人 private repo 无 GitHub-hosted minutes）。
7. 证据层优化（进行中）。

## 2. 已交付功能（Relay Observability 摘要）

- **架构**：`pkg/relay_observer/`（types / normalizer / codec / content / persistence / store_pg / dispatcher / wiring / query / identity）。
  - 两阶段写入：turn 落库 + content 异步入库；fail-open：store 初始化失败 → observer 禁用不影响 relay 主链路。
  - canonical items 带 HMAC digest 去重；context 以 full + delta groups（checkpoint / ordinal）存储，delta 可回溯重建。
  - 预算截断：`reservation = BodyStorage.Size()`，`finishNormalize` head-first 截断 + gap marker。
  - session_id backfill：`UPDATE observer_turns SET session_id = $1 WHERE id = $2 AND session_id IS NULL`。
- **API**：`GET /api/relay-observer/status`（实时快照）、`GET /api/relay-observer/overview`（连续窗口序列，零填充 bucket，`start.Add(windows*winSec)` 时间一致）。
- **UI**：OverviewTab = 4 个 KpiCard（Total Turns / Success Rate / Sessions / Gaps，bar sparkline）+ VChart 窗口趋势 + 窗口表 + 状态卡 + 总计。加载骨架屏、空态、degraded 信封、错误态齐全。
- **测试**：web 236 pass（VChart 在 jsdom 下 mock）；集成测试 `content_pipeline_pg_integration_test.go`（session backfill 断言等）。
- **部署**：sgp2 `/opt/observer-test/` 独立 compose（api/postgres/redis），nginx `observer-test.conf` 反代 127.0.0.1:3100，站点 https://newapi-test.vectorcontrol.tech（凭据仅存本地 secrets 目录，不入库）。

## 3. 仓库拓扑重组（2026-08-02）

| 项目 | 变更前 | 变更后 |
|------|--------|--------|
| 主仓库 | TokenDanceLab/new-api（org，public） | **DeliciousBuding/new-api**（个人，**private**） |
| 旧仓库 | TokenDanceLab/tokendance-gateway | **已删除**（gh api DELETE，exit 0；org 无 newapi 残留） |
| 本地目录 | `D:\Code\TokenDance\tokendance-gateway` | **`D:\Code\TokenDance\newapi`**（mv + `git worktree repair`，所有 worktree 验证正常） |
| remotes | official（QuantumNous/new-api）+ public + legacy | official + **public=DeliciousBuding/new-api**（legacy 已删） |
| 上游 | QuantumNous/new-api（官方，不可变） | 不变 |

验证：`git ls-remote public main` 与本地 HEAD 一致；`gh repo view DeliciousBuding/new-api` 确认 private。

## 4. CI 迁移到 sgp2 自托管 runner

### 4.1 动机

个人 private repo 无 GitHub-hosted 免费分钟（$0 spending limit，hosted runner job 直接失败）。之前 tokendance-analysis 已用自托管 runner，本仓库跟随（用户："我们有 sgp2 的这个私有 runner 了"）。

### 4.2 Runner 部署（sgp2 主机）

- compose：`/opt/ci-runner/compose.yml`，repo 级 runner 服务，`DOCKER_HOST=tcp://dind:2375` 共享 dind，`cache-data` 共享 go 缓存，mem_limit 4g / cpus 2.5。
- 注册：`config.sh --url https://github.com/DeliciousBuding/new-api --token <registration-token> --name sgp2-newapi --labels sgp2 --work _work --unattended --replace`（token 通过 gh api 管道传入，未回显）。
- **并行扩容（2026-08-03）**：单 runner 一次只跑一个 job，Backend/Frontend 排队慢 → 增加 `runner-repo-newapi-2` 服务（同规格），注册 `sgp2-newapi-2`（labels=sgp2）。双 runner online，两 job 并行，检查从排队 10min+ 降到同轮完成。

### 4.3 Workflow 改动与踩坑（均已修复并验证）

| # | 问题 | 修复 |
|---|------|------|
| 1 | main 受 org 继承规则集 "Protect TokenDance source branches" 保护（`deletion` + `non_fast_forward` + `required_status_checks`[Backend/Frontend, strict]），直接 push 被拒 | 全部改动走 PR 流程（正好验证 PR 门禁） |
| 2 | self-hosted 系统 node v18.19.1，rsbuild/@rspack 崩溃（`ERR_INVALID_ARG_TYPE: path undefined`）——hosted runner 预装 node 20+ 掩盖了此问题 | 前端 job 增加 `actions/setup-node` node-version 22 |
| 3 | `pull_request_target` 的 PR Check 用 **base 分支** workflow 文件：合并前跑旧 ubuntu-latest 直接失败（无法复用 hosted runner） | 合并后自动解决；PR #6 验证 pr-quality SUCCESS |
| 4 | `docker-build.yml` 的 `IMAGE_NAME: ghcr.io/tokendancelab/new-api` 还是 org 名——转移后 GITHUB_TOKEN 的 packages:write 只覆盖个人命名空间 | 改 `ghcr.io/deliciousbuding/new-api` |
| 5 | 单 runner 顺序执行慢 | 第二 runner 实例（见 4.2） |

### 4.4 PR 记录

- **#5** `ci: switch PR gates to the sgp2 self-hosted runner`（squash `e79f50fb4`）：pr-check / ci / docker-build 三 workflow `runs-on: [self-hosted, Linux, X64, sgp2]` + node 22 修复。Backend + Frontend 全绿后合并。
- **#6** `ci: fix GHCR image owner after repo transfer`（squash `5dd4a0b31`）：IMAGE_NAME 改个人 owner。Backend / Frontend / pr-quality 三 checks 全绿后合并。

当前 `gh pr list --state open` 为空。main 无遗留。

## 5. 证据层优化（进行中 · 任务 #11）

### 5.1 问题定义（已确认的根因）

- 预算截断：`reservation = BodyStorage.Size()`，但 canonical item 体积 **大于** body（HMAC + JSON 序列化开销）→ 大 agent 请求几乎全部触发截断。
- 截断方向：`finishNormalize` 是 **head-first**（保头截尾），而 agent 的增量内容（delta）在 **尾部** → 最有价值的证据被截掉，agent 大请求几乎全部变成 gap。
- 结论（设计方向已定，待实现）：**tail-first 截断策略**——保尾部 delta 链，头部压缩为 checkpoint。

### 5.2 性能基线（benchmark 已收集）

| 场景 | 耗时 |
|------|------|
| Normalizer responses | ~59µs |
| Normalizer chat | ~22µs |
| Normalizer claude | ~30µs |
| Normalizer large | 0.5–1.1ms |
| Enqueue | ~118ns |

### 5.3 方法论（用户指令约束）

"对于具体的东西 input 设计和 output 应该回到源码去深度解析然后反过来给报告而不是乱搞"——tail-first 预算策略的实现**必须**基于 claude-code / codex 源码事实（agentloop 结构、分支管理、session 追踪、UA 识别、input/output 载荷形态），不得凭猜测设计。

已启动 2 个源码研究 agent（本地仓库 `D:\Code\Projects\claude-code` 与 `D:\Code\Projects\codex`），报告到达后先产出设计报告，再实现。

### 5.4 后续队列（按用户排定的优先级）

- P0：tail-first 预算策略（等研究 agent 报告）
- P1：step_kind 标注、外部 summary（agent 对话产物结构化）

## 6. 安全与边界说明

- 本仓库为 **private**；本文档及仓库内不含任何密钥/口令/token/测试 key 明文——全部敏感凭据只存于本机 `~/.config/server-secrets/`（新api 测试站登录、测试 key、cf token、upcloud api 等），不入 git。
- 审计线数据（身份/会话/请求内容）为敏感用户数据，Root 查询 API 仅 Root 管理员可访问。
- 测试站与 runner 为隔离环境：测试站独立 compose（observer-test-*），不挂生产数据。
- 生产变更确认原则：本会话涉及的生产配置变更（nginx、UFW、compose）均在用户明确授权下执行。

## 7. 待办与风险

- P0 证据层 tail-first 策略实现（阻塞于研究 agent 报告）。
- 风险 a：单 dind 共享——并发构建镜像时队列化（当前仅 newapi repo 使用，风险低）。
- 风险 b：runner 容器重启自愈依赖 entrypoint 对 `.runner` 文件的判断（config 已写入，重启后直接 run.sh，已验证逻辑）。
- 风险 c：ruleset 是 org 继承规则，若 org 规则变更可能影响个人仓库 main 的推送/合并策略。
