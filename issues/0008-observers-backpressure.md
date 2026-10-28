# 观察者扇出与背压策略

**优先级** P0 · **区域** core · **类型** feature · **里程碑** M1

## 背景

OAR 的观察者是"side tap"：同步分发、吞异常、**永不反压 run**。这条规则看起来小，但它决定了一个慢 UI 会不会拖死 agent。Go 的 channel 很容易写出相反的行为（阻塞发送），所以必须显式设计。

## 目标

1. 语义：`Observe(fn)` 默认同步、非阻塞、panic 隔离。
2. 慢消费者：`ObserveBuffered(fn, size, policy)`，策略三选一——`DropOldest` / `DropNewest` / `SignalGap`（记录一条 gap 标记，保证消费者知道丢了）。
3. 永不阻塞 run：任何策略下，写入侧的最坏耗时有上界。
4. 取消：`cancel()` 幂等，取消后不再分发。
5. 指标：`Dropped()`、`LastSeq()` 可查。

## 验收标准

- [ ] 基准测试：1 个慢消费者（每次睡 100ms）+ 10k 记录，run 的 p99 不被拖慢
- [ ] `SignalGap` 的消费者能准确知道缺了 `[a,b]` 区间
- [ ] 并发 `Observe` + `cancel` 无泄漏（goroutine 数收敛）
- [ ] 文档写明"为什么不做反压"

## 非目标

- 网络传输 / SSE（宿主）
- 跨进程 fan-out

## 参考

- OAR `session-kernel.ts` 的 `deliver()`（吞异常、同步）
- 本仓 `docs/architecture.md` §5

## 依赖

#6
