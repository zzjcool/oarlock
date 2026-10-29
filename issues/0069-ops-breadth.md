# Ops 广度：ACP install plans + CheckUpdate 接线

**优先级** P1 · **区域** ops / adapter-acp · **类型** feature · **里程碑** M8

## 背景

`docs/ops.md` 的能力矩阵里，ACP 六家的 `install` 仍全部 absent；Claude/Codex/Cursor 的 `CheckUpdate` 还没接线（只有 `Upgrade`）。这些是 #23 明确留下的 ops 缺口，不需要 vendor 账号即可实现只读 plan/check 和 fake 测试。

## 目标

1. **ACP install plans**：为 grok / kimi / opencode / qwen / qoder / antigravity 实现只读 `InstallPlan`（各 vendor 官方安装命令；opencode 用 npm，其他按 OAR `docs/spec/install.md` 与 runtime install 实现）。`Install` 只在显式调用时执行，并由 re-probe 决定成功；没有安装器的 runtime 保持 absence。
2. **ACP login/auth（能确认的）**：有 vendor CLI 登录/状态命令的（opencode 等）实现 `Login/AuthStatus`；没有可靠通道的保持 absence，不猜。
3. **CheckUpdate**：Claude/Codex/Cursor 接上 vendor 的 check-only 命令；如果只有“升级并看结果”而没有 check，返回 typed `UnsupportedOperation`，不伪造可用更新。`Upgrade` 的版本读回语义不变。
4. 测试：fake 可执行文件覆盖 plan 不执行、install 显式执行、CheckUpdate 成功/失败、无 check 命令的 typed refusal；opencode 的 plan 可 live 只读验证，不实际安装。
5. 更新 `docs/ops.md`、`docs/compat-matrix.md` 对应行；不加第三方依赖；不改其他 adapter 的会话代码。

## 验收标准

- [ ] ACP 六家 install plan 有矩阵/测试；不能安装的明确 absent
- [ ] CheckUpdate 接线后不再返回“未接线”的占位错误；无 check 的 runtime 有 typed 原因
- [ ] plan 纯只读；install/upgrade 只有显式调用才改机器
- [ ] opencode plan 有 live 只读证据（若环境允许）
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- AccountUsage
- 真正执行 ACP 六家安装
- 官方账号 live

## 参考

- `runtime.go`、`internal/ops/*`、`adapters/{acp,claude,codex,cursor}/ops.go`
- OAR `docs/spec/{install,update,login}.md` 与 `runtimes/*/install.ts`、`update.ts`
- issue #23

## 依赖

#23（已合入）
