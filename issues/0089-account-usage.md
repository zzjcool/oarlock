# AccountUsage 接口 + fake HTTP

**优先级** P1 · **区域** ops · **类型** feature · **里程碑** M11

## 背景

`AccountUsageReader` 已在根契约里，但所有 runtime 的 `Runtime.Usage` 都是 nil；`docs/ops.md` 明确记录 absent。这是 #23 唯一完全没有实现的产品能力。没有账号也能先把读取形状、错误分类和 fake HTTP 测试做出来。

## 目标

1. 按 OAR `docs/spec/account-usage.md` 与 `runtimes/{claude,codex}/account-usage.ts` 实现 Claude 和 Codex 的 `AccountUsageReader`：
   - 从 vendor 自己的登录/配置或显式 env 取 token；token 不打印、不入记录、不进错误文本；
   - HTTP 超时/取消；解析 plan、quota window、used/limit、resetsAt；
   - 未登录/无权限/接口不存在 → typed 错误或 absence，不返回空 plan 冒充成功。
2. 用 `httptest` fake 覆盖成功、401、429、缺字段、超时；真实账号 live 不要求。
3. 把 reader 接到 `adapters/claude.Runtime()`、`adapters/codex.Runtime()` 的 `Usage` 字段；更新 `docs/ops.md`、compat-matrix、runtime 文档。
4. 不加第三方依赖；不改会话代码；不伪造 quota；token 从 env/config 读取但绝不写日志。

## 验收标准

- [ ] Claude/Codex 的 AccountUsage 有 fake 成功/失败测试
- [ ] 401/429/超时映射为 typed 错误，不是空数据
- [ ] `Runtime.Usage` 非 nil 且有文档；真实数据标 UNVERIFIED
- [ ] 无 token 泄漏
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 真实账号 live
- Cursor/ACP 的 AccountUsage（无可靠接口）
- 批量机器管理

## 参考

- `runtime.go` 的 `AccountUsageReader`
- `docs/ops.md`
- OAR `docs/spec/account-usage.md`、`runtimes/{claude,codex}/account-usage.ts`

## 依赖

#23（已合入）
