# Fork Delta 清单

最后更新：2026-08-03

> 记录本 fork（TokenDanceLab/new-api）相对 QuantumNous 上游的自有改动落位。
> **每次 agent 开工先读本清单**，避免把功能随意散落进上游核心文件。
> 上游同步模型见下文 §2。

## 1. 自有改动清单

| 模块 | 自有目录 | 上游文件触点 | 是否可上游化 |
|------|----------|--------------|--------------|
| Relay Observer | `pkg/relay_observer` | `service/relay_observation.go`、`controller/relay.go`、`router/relay-router.go` 等 | 部分（旁路审计线，按需评估） |
| **Vision Relay** | **`pkg/vision_relay`** + `setting/model_setting/vision_relay.go` + `service/vision_relay.go` | **`controller/relay.go` 单点钩子（4 行）** | 核心可独立评估（纯核心包无 NewAPI 依赖） |
| CI 私有化 | `.github/workflows` | workflows | 不适用（私有仓库专用） |

### Vision Relay 修改预算（硬规则）

```
必须新增：pkg/vision_relay/**、setting/model_setting/vision_relay.go、
          service/vision_relay.go、docs/plan/vision-relay.md
原有文件最多改一个：controller/relay.go（预扣费后、retry 前单点钩子）
禁止修改（阶段 1）：relay/**、relaykit/**、relaykit/dto/**、relay/channel/**、
          common/body_storage.go、constant/context_key.go、model/option.go、
          controller/option.go、main.go、web/**
```

依赖方向（固定）：

```
controller/relay.go
    ↓
service/vision_relay.go        （Gin/RelayInfo/BodyStorage 事务 + 错误映射）
    ↓
pkg/vision_relay               （纯核心：标准库 + x/image + gjson/sjson，
                                 禁止反向依赖 controller/service/model/setting/relay/Gin）
```

## 2. 上游同步模型

```
public/main             QuantumNous 上游，只读（remote: public）
origin/upstream-main    可选的上游镜像分支
origin/main             集成/生产分支
origin/feat/*           短期功能分支
```

```bash
git fetch public origin
git branch -f upstream-main public/main          # 更新本地上游镜像
git checkout main && git merge --no-ff upstream-main   # 主分支吸收上游，不重写已部署历史
git checkout feat/vision-relay && git rebase main      # 功能分支合并前重放

git config rerere.enabled true                    # 相同冲突复用上次解决
git range-diff <旧上游>..<旧main> <新上游>..<新main>   # 确认自有 patch 语义未在冲突处理中改变
```

## 3. Vision Relay 状态

- 设计：`docs/plan/vision-relay.md`（v0.2.1，GPT 审核 Approved with Implementation Gates）
- 实现：`feat/vision-relay` 分支（提交栈：settings → engine → service → controller → docs）
- 默认关闭（`vision_relay.enabled=false`）；测试环境 sgp2 observer-test 实例（复用其 PG）
