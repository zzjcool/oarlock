# 进程树 / 进程组 / Windows Job / `.cmd`→`.ps1`

**优先级** P1 · **区域** platform · **类型** feature · **里程碑** M1

## 背景

这是所有 Go 实现最容易低估、也最容易在线上炸的部分。multica 为此写了约 900 行并留下多个 issue 编号（E2BIG、Windows 206、`.cmd` 重新分词）。agentexec 的 cursor 只有 79 行，因为它没有承担这些。

## 目标

1. **进程组**：POSIX `Setpgid`，取消时向整组发 SIGTERM，宽限后 SIGKILL。
2. **Windows**：Job Object（`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`）保证孙子进程不逃逸。
3. **`.cmd` 规避**：若入口是 `.cmd`，解析同目录 `.ps1` 直接调用 PowerShell，避免 `%*` 重新分词。
4. **stdin 大输入**：独立 goroutine 写 + 关闭表示 EOF；防止 64KiB 管道死锁。
5. **argv 上限**：识别 `E2BIG` / Windows `206`，转成 `ErrArgvLimit`。
6. **WaitDelay**：父进程退出但孙进程持有 stdout 时，必须有上界。

## 验收标准

- [ ] POSIX：父进程 kill 后，`ps` 中无残留（含孙进程）
- [ ] Windows（CI 或本地）：多行 prompt 经 `.cmd` 路径不被打散
- [ ] 大 prompt（1MB）经 stdin 成功；不出现死锁
- [ ] 超长 argv → `ErrArgvLimit`，含可操作提示
- [ ] `WaitDelay` 场景：wrapper 退出但孙进程存活 → 采集正常结束
- [ ] 三平台 CI 矩阵（linux/macOS/windows）至少编译 + 单元测试通过

## 非目标

- 容器/沙箱隔离
- 跨机器进程管理

## 参考

- multica `cursor_invocation_windows.go`、`argv_limit.go`、`cursor_stdin_unix_test.go`
- OAR `shared/executable/*`

## 依赖

#9
