# Cursor 适配器：CLI + 显式降级

**优先级** P2 · **区域** adapter/cursor · **类型** feature · **里程碑** M3

## 背景

Cursor 的富接口是进程内 JS SDK，Go 用不了。替代是 `cursor-agent -p --output-format stream-json`，其格式**与 Claude 逐帧一致**（agentexec 79 行、multica 生产验证）。缺的是 steer、per-session MCP、插件、登录契约、结构化 usage。

## 目标

1. 复用 Claude 的 stream-json 解析器（**不新增 parser**）。
2. argv：`-p --output-format stream-json [--model] [--resume] [--force | --yolo] [--trust --sandbox disabled] <prompt>`；prompt 走 stdin（不是 argv）。
3. 能力声明（诚实降级）：
   - 不实现 `Steerer`
   - `Refused{mcpServers, launchArgs, ...}` 带原因
   - `Attribution = opaque`（除非实测出 `parent_tool_use_id`）
   - `login` 缺失；文档指向 `cursor-agent login`
4. 未知事件计数 + fixture 回归（防 MUL-5231 式静默丢事件）。
5. resume：CLI 的 chat id，明确与 SDK 的 `agent-<uuid>` 不兼容。

## 验收标准

- [ ] 用 Claude 的 parser 成功解析真实 cursor-agent 流（事件数一致）
- [ ] `thinking` / `tool_call` 顶层事件被正确映射（不是只认 assistant content block）
- [ ] 未经声明的事件类型：计数 + 保留 `Native`
- [ ] 能力表与 `docs/compat-matrix.md` 完全一致
- [ ] 大 prompt（1MB）走 stdin 不死锁

## 非目标

- SDK 能力对等
- 账号/用量 API（CLI 不提供）

## 参考

- agentexec `cursor.go`（79 行）
- multica `cursor.go`、`cursor_stream_fixture_unix_test.go`（MUL-5231 教训）
- ADR-0004

## 依赖

#19（共用 parser）、#16
