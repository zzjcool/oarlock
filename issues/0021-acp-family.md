# ACP 家族：grok / kimi / opencode / qwen / qoder / antigravity

**优先级** P2 · **区域** adapter/acp · **类型** feature · **里程碑** M3

## 背景

6 个 runtime 说同一个协议（Agent Client Protocol，JSON-RPC over stdio），差异在能力协商与厂商怪癖。Go 已有成熟实现（`coder/acp-go-sdk` v0.13.5，支持 client/agent、terminal、permission、extension methods），所以**传输不该自己写**，只写 quirk profile。

## 目标

1. 引入 `coder/acp-go-sdk`，封装为 oarlock 的 `acpFamily`：
   - 初始化 + 能力协商（`sessionCapabilities.additionalDirectories` 等）
   - `session/new` / `session/load` / `session/prompt` / `session/cancel`
   - `session/update` → 统一事件
   - 权限请求 → `toApp` 记录 + 默认 `allow_always > allow_once > cancelled`
   - terminal 请求的生命周期
2. 每家一个 profile（只写差异）：
   - grok：用量视图**不可相加**，选一个权威视图；steer 用 `_meta.sendNow`
   - kimi：只订阅主 agent（子 agent opaque），无 steer
   - opencode v1：子会话不转发（opaque）；v2：`_meta` 里有 child session，归因 nested
   - qwen / qoder：`parent_tool_use_id` 归因；qoder 的 `priority: next`
   - antigravity：子 agent 被扁平化到父 session（不伪造）
3. 统一声明 `AttributionTier`，宁可 `opaque` 也不猜。

## 验收标准

- [ ] 至少 grok 端到端跑通（fixture + live smoke）
- [ ] 权限请求被正确应答并留痕
- [ ] 每家的 `AttributionTier` 有测试证据支撑
- [ ] 6 家共用同一份代码，profile 文件 <200 行/家
- [ ] `unsupported` 的会话选项在 open 前被拒

## 非目标

- ACP 的服务端实现（我们只是 client）
- 远程 ACP（HTTP/WS）——先 stdio

## 参考

- `github.com/coder/acp-go-sdk`、`github.com/ironpark/acp-go`
- OAR `shared/acp/*`、`docs/spec/runtime-matrix.md`

## 依赖

#17、#7
