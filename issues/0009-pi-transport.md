# Pi 传输层：launch + JSONL 关联 + 超时 + 退出

**优先级** P0 · **区域** adapter/pi · **类型** feature · **里程碑** M1

## 背景

Pi 的 RPC 是"一个进程 + 换行 JSON"，命令带 `id`，响应不回带上下文。OAR 的实现只有 49 行（`control.ts`），但它处理了四件事：id 关联、30s 超时、进程退出时拒绝所有 pending、stdout/stderr 分离。

## 目标

```go
type Conn interface {
    Do(ctx context.Context, cmd Command, out any) error   // 写一行，等对应 id 的 response
    Events() <-chan Message                                // 非 response 的全部消息
    Exit() <-chan ExitStatus
    Close() error                                           // 杀进程组
}
```

1. 启动：解析 entry（`command`+`args`，或默认 `rpc-entry.js`），校验 `--version` 与 pin 一致，否则 typed error。
2. 帧：`bufio.Scanner` 需扩大缓冲（默认 64KiB 不够）；超长行要报错而不是静默截断。
3. 超时：单命令 30s，超时即杀进程并让所有 pending 失败。
4. 退出：所有 pending 收到 `ErrExited`，附 stderr 尾部。
5. 生命周期：`Close` 先 SIGTERM 进程组，宽限后 SIGKILL；Windows 用 Job Object。

## 验收标准

- [ ] 假进程测试：不回响应 → 30s 超时 → pending 全失败
- [ ] 假进程测试：写完半行就退出 → 解析器报错，不挂死
- [ ] 大消息：5MB 的单行 JSON 可正确读取
- [ ] 退出码 / stderr 尾部进入记录（作为 `exited` response 的证据）
- [ ] `-race` 干净，无 goroutine 泄漏

## 非目标

- 控制语义（#10）
- 投影（#11）

## 参考

- OAR `runtimes/pi/rpc/{launch,control}.ts`、`shared/executable/*`
- agentexec `jsonlines.go`

## 依赖

#5（spike 已证明方言）、#6
