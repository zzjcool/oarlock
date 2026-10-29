# PromptAndWait 事件驱动 + caller-cancel

**优先级** P2 · **区域** core · **类型** quality · **里程碑** M9

## 背景

`PromptAndWait` 目前每 20ms 轮询 `Records`，行为正确但不够优雅；`Events` 观察者是同步泵，回调不能阻塞。需要改成有界事件订阅（或等价机制），并补 caller-cancel 的明确测试。

## 目标

1. 把 wait 循环改为基于 `Session.Events` 的订阅 + 有界缓冲；保留：
   - `RunCompleted/Failed/Aborted/Interrupted/Rejected` 语义；
   - handled / exited / turn_ended 的优先级；
   - 超时后 abort + 有界等待；
   - 不合成 turn end。
2. caller `ctx` 取消时：abort、等运行时自己的结束、返回 `RunInterrupted` + `ctx.Err()`；`WithTimeout` 触发时返回 `RunInterrupted` + nil error（现状保持）。
3. 补测试：caller-cancel、timeout、晚到的 turn end、handled、rejected；无需账号。
4. 可选 live：Pi 长 turn 中取消 `PromptAndWait`，断言 interrupted + 无第二 turn；失败则明确未验证。
5. 只改 `promptwait.go`/测试与相关 docs；不改 adapter 语义。

## 验收标准

- [ ] 事件驱动实现不阻塞 pump，且无丢记录/丢 turn end
- [ ] caller-cancel 与 timeout 的返回值区分清楚
- [ ] 现有 PromptAndWait 测试全过
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 改变 `RunOutcome` 公开语义
- 其他 runtime 的 wait 实现

## 参考

- `promptwait.go`、`promptwait_test.go`、`observe/observe.go`
- `adapters/pi/live_test.go` 的 PromptAndWait 用例

## 依赖

#44（已合入）
