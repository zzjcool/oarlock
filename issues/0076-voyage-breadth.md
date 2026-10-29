# Voyage 广度：多工具 / abort / subagent / retry

**优先级** P1 · **区域** test · **类型** test · **里程碑** M9

## 背景

#71 已经把单个 bash 工具 turn 做成 live voyage；多工具、abort、subagent、retry 仍未覆盖。它们决定 golden fixture 能否真正防住投影回归。

## 目标

1. 用真实 Pi 录制：
   - 一个 turn 内两个工具调用（bash echo + 第二个命令），断言开始/结束顺序、两个 exitCode、文本顺序；
   - 一个 abort turn（长 turn 中 abort，验证 turn_ended aborted，不合成 turn）。
2. 能安全触发 subagent/retry 就录；不能则明确 `UNVERIFIED`（Pi 是否有 subagent/retry 原生事件以真实流为准）。
3. 每个 fixture 走 voyage 写入/脱敏/索引/体积检查；离线 replay 的 Status/Usage/Context/Conversation 与 live 一致。
4. 更新 `docs/voyage.md`；`OAR_PI_LIVE=1` gated；默认离线。
5. 只改 internal/voyage、testdata/voyage、docs/voyage.md。

## 验收标准

- [ ] 多工具 turn live 录制 + replay PASS，事件顺序与 exitCode 有断言
- [ ] abort turn live 录制 + replay PASS，turn_ended aborted
- [ ] subagent/retry 有 live 证据或明确未验证
- [ ] fixture 过脱敏/索引/体积检查
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 多 runtime voyage
- 压缩 live（Pi RPC 无 compact 控制面，继续保持 UNVERIFIED）

## 参考

- `internal/voyage/{live_test.go,tool_turn_test.go,fixture.go}`
- `testdata/voyage/pi-1.4.0-tool-turn.jsonl`
- issue #71

## 依赖

#71（已合入）
