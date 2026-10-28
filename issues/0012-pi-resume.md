# Pi resume 语义与拒绝证据

**优先级** P1 · **区域** adapter/pi · **类型** feature · **里程碑** M1

## 背景

"同一个 session" 每家定义不同：Claude 是 session id，Codex 是 threadId 且会 replay 全量历史，Pi 是 session **文件**（`~/.pi/agent/sessions/<cwd>/…`），而且 fork 是 transcript 分支而不是子 agent。resume 失败必须有正证据（不存在 / 已归档 / 别处打开），不能把网络错误当成"不可恢复"。

## 目标

1. `Options.Resume` 接受 Pi 的 session id，内部解析为 session 文件。
2. 不存在 → `*SessionNotFound{SessionID, Cause}`；恢复后 seq 从 0 开始（新流实例）。
3. 记录 `system` 类型的历史注入（如果有），不伪造 turn。
4. 与 `dispose` 的交互：resume 后第一次 prompt 必须能接管残留状态。
5. 明确"不读回历史"：只恢复模型上下文，不回放记录（ADR 见 OAR decisions）。

## 验收标准

- [ ] 真机会话：prompt → dispose → resume → 追问，答案体现前文
- [ ] 错误 id → `SessionNotFound`，且 `Cause` 保留原生错误
- [ ] resume 后 `Seq` 从 0 开始，且 `Records(cursor)` 的实例语义有测试
- [ ] 不同 cwd resume 的行为有明确结论（支持 / 拒绝 / 未验证）

## 非目标

- 跨机器 resume
- 历史读回 API

## 参考

- OAR `runtimes/pi/rpc/open.ts`（`piFindSessionFile`）、`docs/spec/session-graph-and-cursor.md`
- multica `ResumeRejected` 的"正证据"原则

## 依赖

#10
