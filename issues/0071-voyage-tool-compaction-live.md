# Voyage 工具/压缩 live 证据

**优先级** P1 · **区域** test · **类型** test · **里程碑** M8

## 背景

#17 的 live voyage 只记录了一个普通 Pi turn；工具调用、压缩等更复杂的事件流还没有“录制→回放→fold 一致”的证据。它们正是 golden fixture 最有价值的部分。

## 目标

1. 用真实 Pi 录制一个工具调用 turn（例如 bash `echo`），写入 voyage，replay 后断言：
   - `Status`、`Usage`、`Context`、`Conversation` 与 live 一致；
   - tool_call_started/ended、exitCode、text 顺序保留；
   - 无网络/进程回放。
2. 压缩：能安全触发（手动 compact 命令或长上下文）就录一条，否则明确 `UNVERIFIED`，不伪造。
3. 新 fixture 放 `testdata/voyage/`，按 #17 的索引/脱敏规则；更新 `docs/voyage.md` 和录制说明。
4. `OAR_PI_LIVE=1` gated；默认测试离线。
5. 只改 internal/voyage、testdata/voyage、docs/voyage.md；不改 adapter 生产代码。

## 验收标准

- [ ] 真实工具 turn 录制 + 离线回放 PASS，fold 结果与 live 一致
- [ ] 工具事件顺序/exitCode 有断言
- [ ] 压缩 live 通过或明确未验证
- [ ] fixture 通过脱敏/索引/体积检查
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 多 runtime voyage
- 数据库/索引服务

## 参考

- `internal/voyage/{live_test.go,fixture.go,voyage.go}`
- `testdata/voyage/*`
- issue #17

## 依赖

#17（已合入）
