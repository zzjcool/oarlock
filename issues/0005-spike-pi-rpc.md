# Spike：Pi RPC 握手 + 一次 turn（Go）

**优先级** P0 · **区域** adapter/pi · **类型** spike · **里程碑** M1

## 背景

团队当前只用 Pi，所以 Pi 是第一个适配器。但 Go 侧没有任何 Pi 实现，且 Pi 的 RPC 入口是 JS（`dist/bundle/rpc-entry.js`，`node >= 22.19`）。**这是全项目最大的未知**：如果 RPC 方言不可控，整个 M1 要重排。

## 目标

用 ≤400 行 Go 证明四件事：

1. 能拉起 `pi --mode rpc`（或等价 entry），换行分帧、逐行读写。
2. 能发一条 `prompt` 并收到 `response`（带 id 关联）。
3. 能收到事件流直到 `agent_settled`，并识别 `turn_end` 的 `stopReason`。
4. 能 `abort` 并在进程不退出时恢复；能 `dispose` 并确认进程组清理。

## 验收标准

- [ ] 一份真实会话的原始 JSONL 转储，提交到 `testdata/pi/`（脱敏）
- [ ] 一个 Go 测试：从该转储回放，断言事件序列与终态
- [ ] 一份 `docs/runtimes/pi.md` 记录：命令集、事件类型、超时行为、退出语义
- [ ] 结论明确：可做 / 有条件可做 / 不可做（附证据）
- [ ] 若失败，写出 M1 的替代路线（例如先做 Claude）

## 非目标

- 完整 session 控制面（#10）
- 投影（#11）
- resume（#12）

## 参考

- OAR `packages/oar/src/runtimes/pi/rpc/{launch,open,control,session}.ts`
- Pi 包：`@earendil-works/pi-coding-agent` 1.4.0

## 依赖

#1（只需要记录的类型定义）
