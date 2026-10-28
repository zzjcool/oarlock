# Pi 状态回读：model / effort / context / mode

**优先级** P1 · **区域** adapter/pi · **类型** feature · **里程碑** M1

## 背景

OAR 的一条硬规则：`model()` / `effort()` 是**运行时报告的**，不是回显请求参数。Pi 在 `get_state` 里给出 `model{provider,id}` 与 `thinkingLevel`；上下文用量要单独发 `get_session_stats`。回显会让宿主的 UI 撒谎。

## 目标

1. `Model()`：从 `get_state` 派生 `provider/id`，作为 `model` 事件入流。
2. `Effort()`：`thinkingLevel` 映射到统一 effort 枚举；Pi 不支持的档位要拒绝（见 #15）。
3. `ContextUsage()`：`agent_settled` 后取一次；`null` 表示未报告。
4. 变更可观察：模型/effort 变化时产生新事件，不只是初始值。
5. 不在 adapter 里缓存：全部从 records 折出（#7）。

## 验收标准

- [ ] 打开后立刻可读，且值与 `pi` 自身显示一致
- [ ] 一次 turn 后 contextUsage 有值；`Seq` 单调递增
- [ ] 模型切换（如果 Pi 支持）产生新 `model` 事件
- [ ] 不支持的 effort 在 open 阶段被拒，不是运行时失败

## 非目标

- 上下文分类明细（Pi 不提供，`ContextBreakdown` 接口不实现）

## 参考

- OAR `runtimes/pi/rpc/session.ts`（`get_state` → model/effort 事件）
- `observe/usage.ts`

## 依赖

#11
