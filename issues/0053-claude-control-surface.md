# Claude 控制面补全：images / Withdrawer / cancel_queued / permissions

**优先级** P1 · **区域** adapter/claude · **类型** feature · **里程碑** M6

## 背景

Claude adapter 的基础对话/steer/abort/resume 已 live 验证；但 `images`、`Withdrawer`、`cancel_queued`、权限应答仍缺失，compat-matrix 已诚实降级。OAR 的 `shared/stream-json` 与 `runtimes/claude` 有对应实现可参考。

## 目标

1. **images**：prompt/steer/queue 的 native image content；Capabilities.Images 与矩阵同步。
2. **Withdrawer**：adapter-held queue 按 inputID 撤回；`not_queued` 语义与 Pi 对齐。
3. **cancel_queued**：使用 init 的 `interrupt_cancel_queued_v1`（若协商到），撤回已写入 CLI 的 queued input；没有该 capability 时明确不支持。
4. **权限应答**：runtime→app 的权限请求可被宿主回答（`toApp` + `answered`），或明确记录为“只记录不应答”的降级；默认 allow_always > allow_once > cancelled，需有证据。
5. 更新 `docs/runtimes/claude.md`、compat-matrix 的 Claude 行；fake + opt-in live（第三方 Anthropic endpoint，权限/图片模型可能不支持时明确 skip）。
6. 不加第三方依赖；不碰其他 adapter。

## 验收标准

- [ ] 每个能力要么有测试 + 真实行为，要么在矩阵/文档明确 unsupported 且有原因
- [ ] 不伪造权限回答/queued 撤回/accepted
- [ ] 现有 steer/abort/resume/usage/context 测试不退化
- [ ] gofmt/vet/test -race/Windows build 全绿
- [ ] live 结果如实写（第三方 endpoint，不是官方账号）

## 非目标

- 官方 Claude 账号 live
- MCP 配置/插件
- 修改冻结的 record/control 语义

## 参考

- `adapters/claude/{session.go,project.go,testdata/*}`
- OAR `shared/stream-json/{session.ts,input-lifecycle.ts,turns.ts}`、`runtimes/claude/*`
- issue #19、#42

## 依赖

#19、#42（均已合入）
