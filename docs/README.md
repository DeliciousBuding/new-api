# Project Documentation

本目录保存源码之外仍需长期维护的架构、配置和操作契约。公开产品介绍与安装入口在项目根 `README.md`；agent 硬边界在 `AGENTS.md`。

## Developer contracts

- [`dev/vision-relay.md`](dev/vision-relay.md)：修改图片接管、视觉 sidecall、缓存或 `vision_relay.*` 设置前阅读；包含失败语义、安全边界和已知实现债务。
- [`dev/relay-observer.md`](dev/relay-observer.md)：修改 Observer capture、PG 存储、查询、retention 或 UI 前阅读；包含 fail-open 隔离、隐私和 schema/query 边界。
- [`dev/client-profile.md`](dev/client-profile.md)：修改 User-Agent/client taxonomy 前阅读；包含不可信 hint、低基数和消费面同步规则。
- [`dev/database-compatibility.md`](dev/database-compatibility.md)：新增 raw SQL、锁或迁移前阅读；包含三库兼容和 Observer PG-only 例外。
- [`dev/billing-safety.md`](dev/billing-safety.md)：修改 quota、媒体乘数或结算路径前阅读；包含饱和算术和审计链。

## Feature and operator references

- [`authentication.md`](authentication.md)：面板 JWT/Refresh Session、多节点 Redis、Cookie OriginGuard 和可信代理契约。
- [`channel/other_setting.md`](channel/other_setting.md)：channel `other` JSON、代理校验和遗留值兼容语义。
- [`installation/BT.md`](installation/BT.md)：BaoTa 安装步骤。
- [`../RELEASE.md`](../RELEASE.md)：fork 分支、CalVer 发布、镜像验证和手工兜底命令。

## Translation references

- [`translation-glossary.md`](translation-glossary.md)：基础术语表。
- [`translation-glossary.fr.md`](translation-glossary.fr.md)：法语术语约定。
- [`translation-glossary.ru.md`](translation-glossary.ru.md)：俄语术语约定。
