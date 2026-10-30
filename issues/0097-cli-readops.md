# CLI 只读 ops：status / usage / update --check / inventory

**优先级** P1 · **区域** cli · **类型** feature · **里程碑** M13

## 目标

在已有 `list/probe/models/run` 上增加四个只读命令，全部复用现有库能力：

```sh
oarlock status [runtime|all] [--json]
oarlock usage [runtime|all] [--json]
oarlock update [runtime|all] --check [--json]
oarlock inventory [runtime|all] [--json]
```

- `status`：`Runtime.Login.AuthStatus`；无 `Login` 时输出 `unsupported`。
- `usage`：`Runtime.Usage.AccountUsage`；无 reader 时 `unsupported`；只读、不伪造 quota。
- `update`：只做 `--check`（调 `Runtime.Upgrade.CheckUpdate`），**不做实际升级**；`--check` 必填。
- `inventory`：`Runtime.Inventory.Inventory`；四段各自 status，保留 partial。
- 所有命令支持 `all`/省略；per-runtime goroutine 并发、保注册表顺序、失败隔离。
- `unavailable` / `unsupported` 不是 error；只有 probe/read 真正失败才整体退 1。
- `--json` 继续用 `oarlock.cli/v1` DTO；运行前错误也输出带 `error` 的 DTO 并退 1。
- 人类默认输出简洁表；`--json` 不打印表。

## 结构

- 只改 `internal/cli`（新文件 + `cli.go` 注册）、`docs/cli.md`、`docs/cli.zh.md`、`internal/cli/*_test.go`。
- 不新增依赖；不改 adapter 语义。
- DTO 定义在 `internal/cli`，不要 marshal 根包 struct。

## 验收

- [ ] 四个命令 human + JSON 都可用
- [ ] 并发/保序/失败隔离有测试
- [ ] unsupported 不算 error，真正 error 退 1
- [ ] 文档中英文更新
- [ ] gofmt/vet/test -race/Windows+darwin build 全绿

## 非目标

- 实际 upgrade/install/login/logout
- orient/mcp
