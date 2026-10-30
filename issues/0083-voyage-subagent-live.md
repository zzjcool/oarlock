# Voyage subagent live：Claude/Codex 子 agent 归因

**优先级** P1 · **区域** test · **类型** test · **里程碑** M10

## 背景

Pi 本机没有 delegating tool，Pi 的 subagent live 无法补；但 Claude 的 `Task` 工具、Codex 的 child thread 可以产生真实的子 agent/子 session 记录。voyage 目前没有 subagent fixture，归因/图边/WithChildren 的 replay 证据不足。

## 目标

1. 用第三方 endpoint（Claude Anthropic-compatible 或 Codex OpenAI Responses，均已跑通）录一个父 turn + 子 agent 的会话：
   - 优先 Claude `Task`；模型不愿调用时提示明确要求；
   - 或 Codex 子 thread（如果更容易）。
2. 写 voyage fixture，离线 replay 断言：
   - `session_linked` 图边真实存在且 parent/child 正确；
   - 子记录带自己的 SessionID / AgentPath，不冒充 root；
   - `Usage.WithChildren` 只加一次子总量；
   - Status 的 root turn 不被子 turn_end 结束。
3. 如果模型/endpoint 无法触发子 agent：明确 `UNVERIFIED`，附原始 tool surface/证据；不伪造 fixture。
4. 只改 internal/voyage、testdata/voyage、docs/voyage.md；必要时加 adapter test helper，不改生产。
5. live gated（`OAR_CLAUDE_LIVE=1` 或 `OARLOCK_LIVE_CODEX=1`），默认离线；token 不入仓。

## 验收标准

- [ ] 至少一个 runtime 的 subagent live fixture + replay PASS
- [ ] graph/AgentPath/WithChildren/root status 断言
- [ ] 无法触发时明确 UNVERIFIED
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- Pi subagent（本机无 delegating tool）
- retry/context overflow

## 参考

- `internal/voyage/*`、`testdata/voyage/*`
- `adapters/claude/{project.go,live_test.go}`、`adapters/codex/{live_test.go,project.go}`
- docs/runtimes/claude.md、codex.md

## 依赖

#71（已合入）
