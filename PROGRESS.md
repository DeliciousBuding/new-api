# PROGRESS — T2.1 versioned session identity aliases (lane A)

开工回执（2026-08-02）：
- 基线已验证：HEAD=7bd208429，git status 干净；SSOT（Session Identity 章节）与 FINDINGS.md 已读。
- 目标：pkg/relay_observer/identity*.go —— Codex/Claude 版本化 HMAC 别名 + scoped 解析，输出 Alias{Version,Digest,Scope} 供 T2.3。
- 顺序：契约测试（红）→ identity.go（绿）→ 全部 gate → SUPER 自评 → commit。
- 最大风险：坏/超预算 Turn-Metadata 头的确定性降级；轮换验证语义；跨 profile 不合并的边界。

## 进度

- [x] 任务0：基线 + 开工回执
- [x] 任务1：红（identity_test.go，18 失败 → 输出已贴）
- [x] 任务1：绿（identity.go，21 测试全 PASS，identity.go 覆盖 94–100%）
- [x] gate：gofmt 无输出；git diff --check OK；GOMAXPROCS=2 go test -p 1 ok（75.9% cov，101 测试）；go vet exit 0；-race 通过（本机未遇预期 error 87，如实记录）
- [x] SUPER 10/10 自评（S/U/P/E/R 各 2 分：隐私契约黄金锁死/契约注释完整/双预算有界/fail-open 确定性/轮换+scope+冲突有界测试锁死）
- [x] commit：feat(observer): versioned session identity aliases (T2.1)

## 关键决策（契约测试锁死，T2.3 消费）

- `Alias{Version,Digest,Scope,Source}`：Digest=64-hex HMAC-SHA-256 全宽（黄金值锁死，非 32-bit 指纹）；Scope 是身份一部分 → 跨 profile 不合并。
- Codex 链：turn_thread > turn_session > header_thread > header_session > cache_key；cache_key 仅 Codex/unknown profile，不覆盖显式 thread。
- Claude 链：claude_session_header > meta_user_session > meta_session。
- 轮换：Verify 按版本选 key（current/previous 两代）；生命周期=安装/window 等字段不入主键。
- 预算：MaxTurnMetadataHeaderBytes=4096、MaxAliasValueBytes=2048，超限来源确定性跳过。
