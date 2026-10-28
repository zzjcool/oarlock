# Pi 投影：事件 → 统一 Event + usage / context

**优先级** P0 · **区域** adapter/pi · **类型** feature · **里程碑** M1

## 背景

Pi 的 `AgentSessionEvent` 是它自己的方言（`agent_start`、`message_update`、`turn_end`、`agent_settled`…）。OAR 用一个纯函数 `foldPiEvent(state, event)` 把它折成统一的 `RuntimeEventBody`，并且**同一个 fold 同时用于 live 和 replay**。

## 目标

1. `foldPiEvent`：纯函数，`(state, native) → (state', []Event)`。
2. 文本：`text_delta` 合并规则；`coalesceText` 为可选读法（不改变底层记录）。
3. 工具：`tool_call_started` / `tool_call_input` / `tool_call_ended`（含退出码）。
4. 终态：`turn_ended{completed|aborted|failed}`，失败带分类与原因。
5. usage：Pi 的统计口径 → `Usage{Total, ByAgent, Unattributed}`；未上报为 `null`。
6. context：`agent_settled` 后主动 `get_session_stats` 取 `contextUsage`。
7. 上下文溢出：`isContextOverflow` 识别为 `failed{context_overflow}`，不当作普通错误。

## 验收标准

- [ ] 黄金转储：一份真实 Pi 会话 → 期望事件序列逐条断言
- [ ] 同一个 fold 既驱动 live 又驱动 replay，输出完全一致
- [ ] 未知事件类型：计数并保留 `Native`，不猜测、不丢弃
- [ ] 文本合并前后，`Seq` 与事件数量关系有明确文档
- [ ] 失败分类覆盖：auth / rate_limit / context_overflow / unknown

## 非目标

- 其他 runtime 的方言（每家一个 profile）
- UI 渲染

## 参考

- OAR `runtimes/pi/projection.ts`、`runtimes/claude/projection.ts`（同为 stream-json 风格）

## 依赖

#9、#10、#7
