# Pi compaction live：用 RPC compact 命令补 voyage 证据

**优先级** P1 · **区域** test / adapter-pi · **类型** test · **里程碑** M10

## 背景

#71 证明了把 `/compact` 当普通 prompt 不会触发压缩；但 Pi RPC 命令集里还有独立的 `compact` 命令（`docs/runtimes/pi.md` 未验证列表）。这是 voyage 目前最后一个硬 UNVERIFIED。无需账号。

## 目标

1. 用 Pi 公共 `Launch`/`Conn` 或 test-only raw 命令发送 `{"type":"compact"}`（先做几轮对话让上下文非空；需要时尝试 idle 与 turn 内两种时机）。
2. 如果 Pi 返回 `compaction_start`/`compaction_end` 或等价原生帧：
   - 录成 `testdata/voyage/pi-1.4.0-compaction-turn.jsonl`；
   - 离线 replay 断言 `compaction_started`/`compaction_ended` 顺序、Status/Usage 不变；
   - 更新 docs/voyage.md。
3. 如果命令不可用/需要更大上下文/无原生帧：明确 `UNVERIFIED`，附原始请求/响应/事件证据；**不伪造 compaction fixture**。
4. 不新增公共 `Action`/API，除非实现确实需要；只改 internal/voyage、testdata/voyage、docs/voyage.md、adapters/pi 测试。
5. `OAR_PI_LIVE=1` gated；默认离线。

## 验收标准

- [ ] `compact` RPC 命令有明确 live 结论（可用/不可用 + 原始证据）
- [ ] 可用则有 fixture + replay 断言；不可用则文档 UNVERIFIED
- [ ] 不伪造 compaction 事件
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 阈值自动压缩（需要打满上下文）
- 公共 compact 控制面

## 参考

- `docs/runtimes/pi.md` §3 命令集
- `adapters/pi/{conn.go,session.go}`
- `internal/voyage/{live_test.go,fixture.go}`
- OAR `runtimes/pi/rpc/session.ts`、`rpc-types.d.ts` 的 compact

## 依赖

#71（已合入）
