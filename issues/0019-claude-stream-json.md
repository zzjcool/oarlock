# Claude 适配器：stream-json（含双向 steer）

**优先级** P1 · **区域** adapter/claude · **类型** feature · **里程碑** M2

## 背景

Claude Code 的 `-p --input-format stream-json --output-format stream-json` 是本项目里**最成熟的一条线**：agentexec、multica、OAR 都在用。它也是唯一提供"回合中插话"（steer）的主流 CLI——靠往 stdin 持续写 user message，并用 `--replay-user-messages` 回来的 echo 确认落点。

同时它是 OAR 里坑最多的适配器之一：`is_error: true` 可能配 `subtype: "success"`；401 会静默重试；usage 有 `result.usage` 与 `modelUsage` 两个重叠视图。

## 目标

1. 启动：`-p --input-format stream-json --output-format stream-json --verbose --replay-user-messages --include-partial-messages`，默认 `--dangerously-skip-permissions`（headless 下权限闸门=挂死）。
2. 双向 stdin：prompt / steer 都写 user message；写入与读取必须解耦，避免死锁。
3. 投影：`system/init`、`assistant`、`user`(tool_result)、`result`、`compact_boundary`、`rate_limit_event`。
4. 终态：`result.is_error` 优先于 `subtype`；失败分类（auth / rate_limit / context_overflow / unknown）。
5. usage：主循环用 `result.usage`，总量用 `modelUsage`；子 agent 的支出进 `Unattributed`（原生不按 agent 归属）。
6. steer：无 echo 确认前不得报 `accepted`；被拒要保留输入处置权。
7. `ContextBreakdown`：Claude 独有，单独接口。

## 验收标准

- [ ] 同一份行为 suite 与 Pi 共用，零 kernel 改动
- [ ] 真实会话 fixture：文本、工具、终态、usage 逐条断言
- [ ] steer 落点：echo 到达才算 accepted；未到达时 `Rejected`
- [ ] `is_error:true` + `subtype:success` 被判定为 failed
- [ ] abort：SIGTERM 进程组，宽限后强杀；`turn_ended: aborted`
- [ ] resume：`--resume <id>`，seq 从 0 开始

## 非目标

- 读回历史（OAR 明确拒绝）
- 权限交互（记录而非应答；headless 下 YOLO）

## 参考

- OAR `runtimes/claude/{launch,projection,session}.ts`、`shared/stream-json/*`
- agentexec `claude.go`

## 依赖

#16、#7、#6
