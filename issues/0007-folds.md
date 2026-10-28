# folds：status / usage / context / conversation

**优先级** P0 · **区域** core · **类型** feature · **里程碑** M1

## 背景

OAR 的读接口全部是 `records()` 的 fold：`status()`、`usage()`、`contextUsage()` 都没有缓存的"快照字段"。这让崩溃恢复、跨进程观察、离线回放天然一致。Go 侧必须同样做，否则一旦引入"adapter 私有状态"，记录流就不再是唯一真相。

## 目标

1. `Status()`：`idle | running(phase) | exited`。不变量：`running` 时记录下来的 prompt 必然 `busy`；`idle` 时必然不 `busy`。
2. `Usage()`：`Total = ByAgent + Unattributed`，子会话有 `WithChildren`；`null` 表示未报告，绝不为 0。
3. `ContextUsage()`：最新上报值 + 对应 `Seq`。
4. `Conversation()`：把记录折成消息视图（user/assistant/tool/reasoning），供 UI。
5. 所有返回值带 `Seq`，可对齐、可重建。

## 验收标准

- [ ] 每个 fold 都是纯函数：`fold(records) == fold(replay(records))`（黄金测试）
- [ ] usage 账本测试：三份重叠视图不可相加的场景（grok 式）必须只取权威视图
- [ ] 子 agent 归因：`ByAgent` 与 `Unattributed` 之和等于 `Total`
- [ ] 崩溃场景：流末尾是半个 tool call 时，`Status` 必须回到可判定状态而不是永久 `running`
- [ ] 空流：`Status=idle`，`Usage=null`，`Context=nil`

## 非目标

- 持久化（宿主）
- 渲染（UI）

## 参考

- OAR `packages/oar/src/observe/{agent-status,usage,conversation,tasks}.ts`
- 本仓 `docs/architecture.md` §4

## 依赖

#6
