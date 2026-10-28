# Codex 适配器：app-server JSON-RPC

**优先级** P2 · **区域** adapter/codex · **类型** feature · **里程碑** M3

## 背景

Codex 的 app-server 是**工作量最大的一块**（OAR 约 2800 行）：握手 → thread → turn，steer 需要 `expectedTurnId`，队列是它自己的（`thread/queue/add`，所以没有 `withdraw`），子 agent 是独立 thread（归因 `nested`），审批是 `toApp` 请求，且子线程通知会走父连接。

## 目标

1. 传输：JSON-RPC over stdio，`initialize` / `initialized` 握手，版本/schema 校验。
2. 会话：`thread/start`、`thread/resume`、`turn/start`、`turn/steer`（带 `expectedTurnId`）、`turn/interrupt`。
3. 事件：`turn/started|completed`、item 流、`thread/tokenUsage/updated`、`contextCompaction`。
4. 队列：使用原生 `thread/queue/add`；`Withdraw` 不实现（接口缺失）。
5. 子 agent：其他 threadId 的通知落为子 session 记录 + 图边；不伪造拓扑。
6. usage：父/子各自上报，`WithChildren` 汇总；父的 `Total` 只算自己。

## 验收标准

- [ ] 握手 + 一次完整 turn 的 fixture
- [ ] steer 的 `expectedTurnId` 不匹配 → 明确拒绝（不是静默）
- [ ] 子 thread 通知进入记录，`AgentPath`/`SessionID` 正确
- [ ] 审批请求以 `toApp` 记录出现，宿主可应答
- [ ] 压缩事件（contextCompaction）映射到 `compaction_started/ended`
- [ ] 版本窗口外 → open 前拒绝

## 非目标

- Cloud/remote codex
- 队列撤回

## 参考

- OAR `runtimes/codex/*`（约 2790 行）、`docs/spec/runtime-matrix.md`
- Codex app-server schema（pin 到某个 commit）

## 依赖

#16、#7、#17
