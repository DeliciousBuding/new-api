# Release Process

TokenDance Gateway 发布流程：`dev`（fork 主线兼发布线）→ tag → GHCR image。

## 分支模型（2026-08-11 起，双分支）

- `main` = upstream mirror（`upstream-sync.yml` 每日 02:00 自动同步 + 手动触发；失败自动开 issue 告警）。禁止直接提交。
- `dev` = **默认分支**，fork 唯一主线 = 发布线。功能分支（`fix/`、`feat/`、`docs/`、`chore/`）PR 进 `dev`；发布 tag 直接从 `dev` 打。
- `dev` 启用了分支保护：合并前必须通过 CI（Backend + Frontend）required checks；禁止 force push。

## 发布步骤（一次完整发布）

```bash
# 1. feat → dev
gh pr create --base dev --head <branch> --title "<summary>" --body-file <body.md>
gh pr merge --merge <pr-number>
```

PR body 使用 `.github/PULL_REQUEST_TEMPLATE.md`；如提交者是 AI 协助，需在 body 中声明。

## 打 tag + 构建镜像（自动化）

```bash
# 预演：只看下一个 tag 是什么，不推送
gh workflow run release-tag.yml -f dry_run=true

# 正式发布：自动计算 v1.0.0-td-YYYYMMDD.N 并推送 tag + dispatch docker-build.yml
gh workflow run release-tag.yml

# 也可显式指定 tag
gh workflow run release-tag.yml -f tag=v1.0.0-td-20260811.2
```

`release-tag.yml` 会：
1. 从 `dev` 分支解析 tag（今日已有 tag 则序号 +1；已存在的 tag 拒绝重发）
2. push tag → dispatch `docker-build.yml` → 并行构建 amd64（`ubuntu-latest`）+ arm64（`ubuntu-24.04-arm` 原生 ARM runner，public repo 免费），完成后合并多架构 manifest 并推送
   `ghcr.io/deliciousbuding/new-api:<tag>`
3. 在 run summary 输出镜像验证命令

> 注意：CI/推送触发的事件由 `GITHUB_TOKEN` 发出时不会再次触发 workflow（防递归），因此 orchestrator 显式 dispatch `docker-build.yml`。

## 验证镜像

```bash
docker manifest inspect ghcr.io/deliciousbuding/new-api:<tag>

# 查看该包所有 tag（正确端点：user 命名空间）
gh api users/DeliciousBuding/packages/container/new-api/versions \
  --jq '[.[].metadata.container.tags[]?] | select(. != null)'
```

`docker-build.yml` 对已发布的 tag 拒绝重建（tag 不可变）；重发必须用新 tag。

## 手工兜底（仅自动化故障时）

```bash
git checkout dev && git pull
git tag v1.0.0-td-$(date +%Y%m%d).1 && git push origin <tag>
# 再在 GitHub Actions 手动运行 docker-build.yml，输入 tag
```

## 已知治理设置（2026-08-11）

- `delete_branch_on_merge=true`：PR 合并后自动删除 head 分支
- `allow_auto_merge=true`：PR 满足 required checks 后可 auto-merge
- 分支保护：`dev` 要求 CI 两个 job 通过，禁止 force push
- 上游同步失败会开 issue：`upstream-sync failed — main mirror drift`
