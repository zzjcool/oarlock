# 错误与拒绝码：8 种 typed code + 两类错误

**优先级** P0 · **区域** core · **类型** spec · **里程碑** M0

## 背景

OAR 的分支判据是**词，不是文案**：`busy / no_active_turn / not_queued / unsupported / runtime_exited / disposed / runtime_refused / error`。这条规则让宿主不用解析 vendor 的英文。Go 里对应的是 `errors.Is/As` + 常量。

## 目标

```go
type Code string
const (
    Busy Code = "busy"
    NoActiveTurn Code = "no_active_turn"
    NotQueued    Code = "not_queued"
    Unsupported  Code = "unsupported"
    RuntimeExited Code = "runtime_exited"
    Disposed     Code = "disposed"
    RuntimeRefused Code = "runtime_refused"
    ErrorCode    Code = "error"
)

type Rejected struct{ Code Code; Reason string; Seq uint64; RequestID string }
type UnsupportedOption struct{ Option, Reason string }
type SessionNotFound struct{ SessionID string; Cause error }
```

规则：
1. 控制结果只有 `accepted` / `rejected` 两种；**最终状态永远是事件，不是返回值**。
2. `unsupported` 表示"这次做不到（输入层面）"；整件事做不到 = 接口缺失。
3. 拒绝必须保留调用方对输入的处置权（不吞、不重试、不代发）。
4. 包装链保持 `errors.Is` 可用（E2BIG、超时、进程退出）。

## 验收标准

- [ ] `docs/spec/errors.md` + `errors_test.go` 的期望表
- [ ] 每个 code 配一个触发场景与调用方应有的动作
- [ ] 明确"进程退出"映射：`exited` 响应 + `runtime_exited`
- [ ] 明确 abort 被拒（turn 恰好结束）与 abort 被接管的区别

## 非目标

- vendor 失败分类（auth/rate_limit/context_overflow）——这是 `TurnOutcome.Failed` 的事，见 #7

## 参考

- OAR `docs/spec/record-stream.md#record-contracts`、`contracts/errors.ts`

## 依赖

#1
