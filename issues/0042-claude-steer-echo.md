# Claude live steer echo：定位、修复或明确记录 CLI 限制

**优先级** P0 · **区域** adapter/claude · **类型** fix · **里程碑** M5

## 背景

#19 按 OAR 语义实现了“steer 只有 echo 到达才 accepted”。离线 fixture 通过；本机 `claude 2.1.312` + 第三方 Anthropic-compatible endpoint 下，live steer 在 10 秒内没有 echo，adapter 正确地返回 `rejected{error}`，输入仍归调用方。需要判断是 adapter 的 user message 格式、Claude 版本的 `--replay-user-messages` 行为，还是第三方 endpoint/模型的限制。

## 目标

1. 抓取 live steer 前后的 stdin/stdout 原始帧（脱敏），与 `adapters/claude/testdata/steer-lifecycle.jsonl` 对比。
2. 检查 `--replay-user-messages` 在 2.1.312 下是否仍对 mid-turn user message 产生 `isReplay` 回显；检查 user message 是否需要 `session_id` / `parent_tool_use_id`。
3. 若 adapter 侧可修：修复并让 live 测试通过；若确认是 CLI/endpoint 限制：保留 rejected 语义，live 测试 skip 时打印精确原因，文档记录，**不得伪造 accepted**。
4. 增加 Claude 版 `TestLivePromptAndWait`（公共 API），与 #24 的 Codex/Cursor 版本对齐。
5. 更新 `docs/runtimes/claude.md` 与 compat-matrix 的 steer 行，注明证据等级。

## 验收标准

- [ ] live steer 要么 PASS（有真实 echo），要么有可复现的“CLI/endpoint 不回显”证据与明确 skip 原因
- [ ] 未收到 echo 时仍返回 rejected，输入处置权不变
- [ ] Claude `TestLivePromptAndWait` PASS 或明确未验证
- [ ] 离线 steer/replay 测试不退化
- [ ] `gofmt`/`vet`/`test -race`/Windows build 全绿

## 非目标

- 官方 Claude 账号 live
- images / withdraw / cancel_queued

## 参考

- `adapters/claude/{session.go,live_test.go,testdata/steer-lifecycle.jsonl}`
- OAR `shared/stream-json/session.ts` 的 steer 实现
- #19、#41

## 依赖

#19（已合入）
