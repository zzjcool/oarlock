# Claude cancel_queued live + 权限请求 headless 结论

**优先级** P1 · **区域** adapter/claude · **类型** test · **里程碑** M7

## 背景

#53 的 `cancel_queued` 选项只有 fake 证据；权限应答默认是 record-only，headless `--dangerously-skip-permissions` 下不会产生 `can_use_tool`。本 issue 用第三方 Anthropic-compatible endpoint 把能 live 的部分补齐，不能 live 的写成明确结论。

## 目标

1. **cancel_queued live**（`OAR_CLAUDE_LIVE=1`，第三方 endpoint）：
   - 长 turn 中 queue 一条输入；
   - abort 时带 `cancel_queued`（仅当 `system/init` 声明 `interrupt_cancel_queued_v1`）；
   - 断言被取消的 queued input 不会成为下一轮 turn，且记录里有取消证据；没有 capability 时退回普通 interrupt。
2. **权限结论**：
   - 默认 headless YOLO 下权限请求不可产生，明确写成 record-only/不可 live，不伪造 `answered`；
   - 若能安全地以非 YOLO 模式触发 `can_use_tool`，补一条 live；否则明确未验证。
3. 更新 `docs/runtimes/claude.md` 与 compat-matrix：`cancel_queued`/permission 的证据等级如实标注。
4. 只改 adapters/claude + 该 runtime 文档/矩阵行；不加第三方依赖。

## 验收标准

- [ ] cancel_queued live 有明确结果（生效/未触发/无 capability），原始帧可追溯
- [ ] 被取消的输入没有变成下一轮 turn
- [ ] 权限请求的 headless 结论写清楚，不伪造 answered
- [ ] 现有 steer/abort/queue/images/withdraw 测试不退化
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 官方 Claude 账号 live
- MCP/plugin 配置
- 权限交互产品化

## 参考

- `adapters/claude/{session.go,permission.go,live_test.go}`
- OAR `shared/stream-json/{session.ts,turns.ts}`
- issue #53

## 依赖

#53（已合入）
