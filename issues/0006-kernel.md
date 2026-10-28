# kernel：append / observe / cursor / reachability

**优先级** P0 · **区域** core · **类型** feature · **里程碑** M1

## 背景

OAR 把所有 adapter 共用的部分收在 `session-kernel.ts`（215 行）：seq 分配、保留日志、观察者扇出、控制记录、可达性判定。每个 adapter 只负责"把帧推进来"和"决定接受还是拒绝"。这是让 10 个适配器不各写各的关键。

## 目标

```go
type Kernel interface {
    Frame(body FrameBody, at RecordAt) Record
    Request(dir Direction, body RequestBody, at RecordAt) RequestRecord
    Respond(requestID string, body ResponseBody, at RecordAt) ResponseRecord
    Control(body RequestBody, decide func(RequestRecord) ResponseBody, at RecordAt) ControlOutcome
    Unreachable() *Rejected            // 从流里推，不看标志位
    Records(Cursor) []Record
    Observe(fn func(Record)) (cancel func())
    Graph() SessionGraph
}
```

要求：
1. `Seq` 稠密递增；单实例内不跳号、不回退。
2. `Control` 先记录 request，再执行 decide，再记录 response；decide panic → `error` 拒绝，不崩进程。
3. 观察者同步分发、吞掉 panic、**永不影响 run**。
4. `Unreachable` 由 `exited` response 或 `dispose` request 推导。
5. 空 prompt/steer/queue（无图片）在 decide 之前拒绝。

## 验收标准

- [ ] 100% 覆盖五条不变量（每条一个测试，命名即断言）
- [ ] 并发测试：8 个 goroutine 同时 append，seq 无重复无空洞
- [ ] `-race` 干净
- [ ] 观察者在分发中 panic 不影响后续观察者与 run
- [ ] 与 #1 的规范逐条对照

## 非目标

- 落盘、持久化、索引
- 多 session 复用同一实例

## 参考

- OAR `packages/oar/src/shared/session-kernel.ts`、`seal-session.ts`

## 依赖

#1、#2、#3
