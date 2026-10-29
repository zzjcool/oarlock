# 跨 runtime 基础对话 live 矩阵：PromptAndWait + Status

**优先级** P0 · **区域** core/test · **类型** test · **里程碑** M5

## 背景

公共 `oarlock.PromptAndWait`、`observe.StatusOf` 和 `Require` 已实现；Pi 有 `TestLivePromptAndWait`。Claude/Codex/Cursor 仍只有 adapter 内部测试，缺“公共 API 跨 runtime 的基础对话 + 状态”证据。本机第三方 endpoint 同时支持 Anthropic Messages 与 OpenAI Responses，可以在不登录 vendor 账号的情况下跑真机。

## 目标

1. 在 **Codex** 和 **Cursor** 增加 `TestLivePromptAndWait`（opt-in；Claude 版由 #42 负责，避免同一文件冲突）。
2. 每个测试走公共 API：`Require` / `Runtime` → `Open` → `PromptAndWait(WithTimeout)` → `observe.StatusOf` → `Dispose`。
3. 断言：`RunCompleted`、非空 `Text`、`Turn.Kind == completed`、`Status == idle`、`Seq` 单调、Dispose 幂等。
4. 新增 `docs/live-testing.md`：第三方 Anthropic/OpenAI-compatible endpoint 的配置方法（env / CODEX_HOME），明确 token 只从环境读取、不入仓。
5. 所有 live 测试默认 skip；`go test -race ./...` 与 CI 不依赖凭证。

## 验收标准

- [ ] Codex `TestLivePromptAndWait` 在 `CODEX_HOME` 自定义 provider + `OPENAI_API_KEY` 下 PASS
- [ ] Cursor `TestLivePromptAndWait` 在本机已登录的 `cursor-agent` 下 PASS
- [ ] 两个测试都断言 completed / 非空 text / status idle / turn outcome
- [ ] `docs/live-testing.md` 可按步骤复现，且不含任何真实 token
- [ ] `gofmt -l .` 空、`go vet ./...`、`go test -race ./...`、`GOOS=windows go build ./...` 全绿

## 非目标

- AccountUsage、ACP live、Claude steer live（各自 issue）
- 官方 Claude/Codex 账号的 live
- 新的 adapter 能力

## 参考

- `promptwait.go`、`observe/observe.go`、`adapters/pi/live_test.go`
- 本机 `~/.pi/agent/auth.json` 的 any-endpoint（只读，不复制 token）
- `adapters/codex/live_test.go`、`adapters/cursor/live_test.go`

## 依赖

无（#42 负责 Claude 的同类测试）
