# Pi steer：按 OAR RPC 实现 Steerer（P1 决策项）

**优先级** P1 · **区域** adapter/pi · **类型** feature · **里程碑** M5

## 背景

issue #10 当时写下“Pi RPC 无原生 steer → 不实现 Steerer”，但 OAR 的 `runtimes/pi/rpc/session.ts` 明确发送 `{type:"steer", message}`，compat-matrix 原本也是 ✅。这是已知的 issue/参考实现冲突。按“有问题参考 OAR”的原则，先在 Pi 1.4.0 上验证 RPC steer，再决定是否恢复 `Steerer`。

## 目标

1. 用真实 Pi 1.4.0 发 `steer`，确认它进入当前 turn（不是新 turn、不是 queued）。
2. 若可用：实现 `Steerer`，写 fake Conn 测试 + opt-in live 测试；更新 compat-matrix 为 ✅；`Capabilities` 不变。
3. 若不可用：保留编译期缺失，记录真实流/错误证据；更新 issue #10 的结论说明（不修改历史 issue，写在本 issue 报告）。
4. 不得合成 turn end；steer 的最终状态仍以运行时自己的事件为准。

## 验收标准

- [ ] live Pi steer 有明确结论（可用/不可用 + 原始帧证据）
- [ ] 若实现：`*pi.Session` 满足 `oarlock.Steerer`；fake/live 测试通过
- [ ] 若不可用：compat-matrix 与文档明确 ❌ 及原因
- [ ] 不引入第三方依赖
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- images / withdraw（另开 issue）
- 其他 runtime 的 steer

## 参考

- OAR `runtimes/pi/rpc/session.ts`
- `adapters/pi/session.go`、`testdata/pi/*`
- issue #10、`docs/compat-matrix.md`

## 依赖

#10（已合入）
