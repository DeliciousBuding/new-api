# TokenDance Gateway / 词元跳动 API 网关

TokenDance Gateway 是面向 AI Agent、开发者和团队的模型 API 网关，提供多供应商路由、模型访问控制、API key 与额度治理、用量记录，以及多种常见模型协议的兼容入口。

本仓库是 TokenDance Gateway 的 New API 运行时 fork。New API 的上游项目身份、QuantumNous 归属、许可证和自托管部署入口以[项目根 README](../../README.md)为准；本页只定义 TokenDance Gateway 的公开接入契约，不替代上游部署文档或完整协议文档。

## 公开 API 地址

公开客户端必须使用以下 canonical API base：

```text
https://api.tokendancelab.com/v1
```

兼容 OpenAI SDK 的客户端通常可将 `base_url` 或 `baseURL` 设置为该地址。协议兼容表示请求与响应格式兼容，不表示该服务由对应协议厂商运营。

## 两类凭据不可混用

| 凭据 | 用途 | 使用方式 | 禁止用法 |
|---|---|---|---|
| TokenDance API key | 调用 `/v1` 下的模型 API | `Authorization: Bearer $TOKENDANCE_API_KEY` | 不得用于浏览器、Dashboard 或管理后台登录 |
| TokenDance ID / OIDC 会话 | Dashboard 与管理后台的用户登录 | 由浏览器完成 OIDC 登录和会话建立 | TokenDance ID access token 不得作为模型 API key |

`$TOKENDANCE_API_KEY` 应由环境变量或 secret manager 注入。不要把实际 API key 写入源码、命令历史、日志、Issue 或文档。

## 快速开始

以下命令适用于 Bash、zsh 等兼容 shell。先通过 `/models` 查询当前 API key 可访问的模型，再使用返回的模型 ID 发起请求。

### 查询模型

```bash
curl "https://api.tokendancelab.com/v1/models" \
  --header "Authorization: Bearer $TOKENDANCE_API_KEY"
```

模型可见性取决于 API key、用户或团队、额度、供应商健康状态和路由策略。`/models` 返回模型不代表该模型支持所有协议、模态或可选字段。

### Chat Completions

将 `MODEL_ID_FROM_MODELS_RESPONSE` 替换为 `/models` 返回且适用于 Chat Completions 的模型 ID。

```bash
curl "https://api.tokendancelab.com/v1/chat/completions" \
  --header "Authorization: Bearer $TOKENDANCE_API_KEY" \
  --header "Content-Type: application/json" \
  --data '{
    "model": "MODEL_ID_FROM_MODELS_RESPONSE",
    "messages": [
      {
        "role": "user",
        "content": "Hello from TokenDance Gateway"
      }
    ]
  }'
```

## 多协议兼容说明

TokenDance Gateway 可承载多种协议族，实际可用范围由模型、API key 权限和运行时路由共同决定：

- **OpenAI-compatible**：包括 Chat Completions、Responses，以及按模型开放的 embeddings、图像、音频和 Realtime 能力。
- **Claude Messages-compatible**：使用对应的 Messages 端点和字段语义；不要假定 Chat Completions 的所有字段都能无损映射。
- **Google Gemini-compatible**：使用对应的 Gemini 端点和请求语义；工具调用与多模态能力以具体模型为准。
- **Rerank 与其他专用接口**：仅在 API key 获得相应模型和接口权限时可用。

协议族、模型可见性和具体能力是三个不同维度。客户端应选择目标协议的端点，并按 API 响应处理不支持的字段或能力；完整端点和字段定义见[上游 API 文档](https://docs.newapi.pro/en/docs/api)。

## 用户可见错误类别

公开文档将错误归入以下五类。具体 HTTP 状态码和机器可读错误码以 API 响应及上游协议文档为准。

| 类别 | 含义 | 建议操作 |
|---|---|---|
| Authentication（认证） | API key 缺失、无效、过期或已停用 | 检查 Bearer header；必要时轮换 TokenDance API key |
| Quota（额度） | 用户、API key、团队、分组或订阅额度不足 | 检查用量与额度，补充额度或申请合适的访问范围 |
| Model access（模型访问） | 当前 API key 无权访问请求的模型或接口 | 重新查询 `/models`，选择已启用且协议匹配的模型 |
| Provider health（供应商健康） | 上游暂时降级、限流或不可用 | 使用指数退避重试，或切换到其他已启用模型 |
| Policy（策略） | 请求被路由、滥用防护或安全策略拒绝 | 检查请求和适用政策；不要原样高频重试 |

面向用户的错误说明不得公开内部供应商账号、路由拓扑、主机或故障切换细节。

## 公共状态词汇

公开状态页和用户通知只使用以下六个状态词：

| 状态 | 含义 | 用户操作 |
|---|---|---|
| Operational（运行正常） | API endpoint 和主要路由可用 | 正常使用 |
| Degraded provider（供应商降级） | 一个或多个上游变慢、限流或不可用 | 退避重试或选择其他已启用模型 |
| Partial outage（部分中断） | 某个 API surface、模型组或适配器不可用，其他能力仍可用 | 使用未受影响的能力，等待状态更新 |
| Quota limited（额度受限） | API key、用户、团队、分组或方案额度阻止请求 | 检查用量与额度，或申请访问 |
| Maintenance（维护） | 计划内操作可能影响管理面或模型路由 | 遵循维护窗口说明 |
| Incident（事件） | 未计划的服务影响正在调查 | 关注状态更新，避免激进重复请求 |

`Quota limited` 描述调用方的访问或额度状态，不等同于平台故障。

## 公开与私有运维边界

公开材料可以描述 canonical API base、凭据边界、协议类别、模型访问条件、用户可执行的错误处理和以上六个状态词。

以下内容只属于私有运维文档，不得复制到公开 README、API 示例、Issue 或状态通知：

- 实时主机、IP、端口、origin、容器、网络和 compose 路径；
- 管理员密钥、供应商凭据、上游账号、数据库与缓存连接信息；
- 日志、备份位置、监控探针、故障切换、回滚和灾难恢复命令；
- 能推断内部拓扑、供应商账号或安全控制的事件证据。
