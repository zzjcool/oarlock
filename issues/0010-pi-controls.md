# Pi session 控制面：prompt / queue / abort / dispose

**优先级** P0 · **区域** adapter/pi · **类型** feature · **里程碑** M1

## 背景

OAR 的 Pi 适配器实现了一个容易被忽略的事实：Pi 的 `prompt` 可能**不开始一个 turn**（slash command 或扩展处理了它，`disposition: "handled"`）。此时不能伪造 turn end，也不能把会话卡在 `running`。

## 目标

1. `Prompt`：busy 时拒绝（不隐式排队）；`disposition=handled` 时立即恢复 idle 且不产生 turn 事件。
2. `Queue`：适配器自己持有（`durable=false`），turn 结束后 drain；队列项被拒要记录原因。
3. `Abort`：发 `abort` 命令；若 turn 在宽限期内未结束，接管并杀进程（记录 `accepted` 而非"已退出"）。
4. `Dispose`：先尝试正常结束，超时后强杀；幂等；已退出时立即 `accepted`。
5. `Steer`：Pi RPC 无原生 steer → 不实现 `Steerer` 接口（编译期缺失）。

## 验收标准

- [ ] 状态机测试：idle → prompt → busy 拒绝 → turn end → idle
- [ ] `handled` 场景：不产生 `turn_ended`，`Status` 回到 idle，`Seq` 连续
- [ ] abort：turn 内 abort → `turn_ended: aborted`；turn 恰结束 → 仍是 completed
- [ ] abort 后进程卡死 → 宽限期后杀掉，records 里有 `exited`
- [ ] dispose 幂等；已 exited 时 `accepted`
- [ ] 队列：2 条排队 → 依次执行 → 每条有独立 turn

## 非目标

- resume（#12）
- 多并发 turn（不变量禁止）

## 参考

- OAR `runtimes/pi/rpc/session.ts`（`start`/`drain`/`abort`/`pendingAbort`）
- `shared/abort-fallback.ts`

## 依赖

#9
