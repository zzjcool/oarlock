# CLI 可靠性：crash-safe `--record` + 进程级测试

**优先级** P1 · **区域** cli / test · **类型** quality · **里程碑** M13

## 背景

Phase 1 的 `--record` 在 run 正常结束后才落盘；进程崩溃/第二次 SIGINT 可能没有日志。且 CLI 目前主要是 DTO/渲染单测，缺少“构建二进制后”的退出码/信号/record 行为测试。

## 目标

1. 把 `--record` 改为**运行期间增量写**：
   - session 打开/首条 record 出现后创建 voyage writer（header 需要 sessionId）；
   - 按 Seq 增量追加 records，保留 `O_EXCL` 不覆盖；
   - 正常结束时写 end（reason 用实际 outcome，如 completed/failed/aborted/rejected/disposed）；
   - 进程被第二次 SIGINT 或崩溃时，已写前缀 + header 保留；无 end 表示截断；
   - 不改变 `--record` 已有路径直接失败、不启动 session 的语义。
2. 增加 CLI 进程级测试：
   - 构建 `cmd/oarlock` 到临时路径，跑 `list` / `probe`（可用 fake runtime 或真实 Pi probe，不要求 live turn）；
   - 验证 `--record` 已存在退 1、正常 run 产生 header/records/end；
   - 验证 `--json` 输出是合法 DTO；
   - 信号行为可用可注入 signal 的 fake 测试，不做真实 live。
3. 只改 `internal/cli`、`cmd/oarlock` 测试、`docs/cli.md`/`docs/cli.zh.md`；不改 adapter 语义。
4. 不加第三方依赖。

## 验收

- [ ] `--record` 在 run 完成前已写入 header + 部分 records（测试可见）
- [ ] 正常结束有 end；已存在路径不覆盖
- [ ] 至少 2 个进程级 CLI 测试
- [ ] 文档更新“增量写/截断”
- [ ] gofmt/vet/test -race/Windows+darwin build 全绿

## 非目标

- 交互登录
- orient/mcp
