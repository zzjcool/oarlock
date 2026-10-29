# Pi withdraw live + Cursor 显式 --resume live

**优先级** P1 · **区域** adapter/pi, adapter/cursor · **类型** test · **里程碑** M7

## 背景

Pi 的 `Withdrawer` 已在 #52 实现，但只有 fake/离线证据；Cursor 的 `--resume` 只有同 chat 第二次 turn 的 live 证据，没有显式 resume 一条 chat id 的测试。两者都不需要账号：Pi 已登录，Cursor 已登录。

## 目标

1. **Pi withdraw live**（`OAR_PI_LIVE=1`）：
   - 开真实 Pi，prompt A 进入 running；
   - queue B，随后 `Withdraw(inputID)` 在 drain 前撤回；
   - 断言 `accepted`、B 从未出现在 wire/records 的 prompt 中、未知 inputID 返回 `not_queued`、队列记录不被改写；
   - dispose 后无残留。
2. **Cursor 显式 resume live**（`OAR_CURSOR_LIVE=1`）：
   - 第一轮拿到 chat id，dispose；
   - 用 `Options.Resume=<chat id>` 重新打开，新流 Seq 从 1；
   - 第二轮追问体现第一轮上下文；记录 `--resume` argv 证据；
   - 失败/不支持时明确 skip 并写清原因。
3. 更新 `docs/runtimes/pi.md`、`docs/runtimes/cursor.md`、compat-matrix 对应行；live 证据如实标注。
4. 只改 adapters/pi、adapters/cursor 及相关文档；不加第三方依赖。

## 验收标准

- [ ] Pi withdraw live PASS：accepted / 未发送 / not_queued 三态
- [ ] Cursor 显式 resume live PASS 或明确未验证原因
- [ ] 新流 Seq 从 1，旧 chat 上下文保留
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- Pi images（#52 已做）
- Cursor steer/MCP
- 官方账号行为

## 参考

- `adapters/pi/{withdraw.go,live_input_test.go}`
- `adapters/cursor/{live_test.go,session.go,open.go}`
- issue #52、#22

## 依赖

#22、#52（均已合入）
