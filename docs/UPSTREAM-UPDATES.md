# 快速同步上游版本

目标：只检查本次上游修改实际命中的本地定制和一跳关联文件，不重复通读全仓库。

## 日常只用三条命令

### 1. 更新前生成影响报告

```bash
pnpm upstream:impact
```

该命令会：

- 获取最新 `upstream/dev`；
- 比较上次同步的 upstream SHA 与最新 SHA；
- 只列出命中的本地定制、直接修改文件、一跳关联位置和必须保持的行为；
- 给出对应的定向测试，不修改业务代码。

如果没有命中本地定制，正常合并并依赖全量 CI 收尾即可，不需要扩大人工审查。

### 2. 合并和修复后运行定向检查

```bash
pnpm upstream:check
```

只运行本次命中功能登记的测试和前端检查。定向检查通过后，仍应让 GitHub Actions 的 Workspace Quality、GHCR Image、CodeQL 和 Commit Message 完成最终验证。

### 3. 全部验证完成后记录新基线

```bash
pnpm upstream:baseline
```

脚本只允许把当前 `HEAD` 已包含的 `upstream/dev` 写为新基线。随后把 `docs/maintenance-manifest.json` 与本次同步结果一起提交。

## 清单维护规则

清单位于 `docs/maintenance-manifest.json`，只保存四类信息：

- 本地定制的文件锚点；
- 命中后需要查看的一跳关联位置；
- 不可破坏的业务约束；
- 定向检查命令。

只有本地定制新增、移动或约束变化时才更新清单。普通上游版本同步只更新 `lastSyncedCommit`。

## 可选参数

```bash
pnpm upstream:impact -- --no-fetch
pnpm upstream:impact -- --all-files
pnpm upstream:impact -- --base <旧SHA> --target <新SHA>
```

- `--no-fetch`：离线使用已有 upstream 引用；
- `--all-files`：需要时才输出全部变更文件；
- `--base` / `--target`：临时审查任意两个提交，不改变清单。

## 边界

脚本负责缩小人工审查范围，不代替编译器和全量 CI。接口、数据库状态、异步任务、上传或安全相关文件一旦命中，仍按报告中的关联位置和不变量检查。
