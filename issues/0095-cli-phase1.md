# CLI Phase 1：list / probe / run / models（DTO + --cwd + 不覆盖 record）

**优先级** P1 · **区域** cli · **类型** feature · **里程碑** M12

## 决策（来自用户）

1. `run` **要** `--cwd`。
2. `--json` 输出**稳定 DTO**，不直接暴露 `oarlock.Record` / 内部 struct。
3. `--record` 与现有 `voyage.Create` 语义**保持一致**：`O_EXCL`，**拒绝覆盖已有文件**。

## 目标

新增一个真正的 `oarlock` 二进制，Phase 1 只做四个命令：

```sh
oarlock list
oarlock probe <runtime|all> [--json]
oarlock models <runtime|all> [--json]
oarlock run <runtime> <prompt> \
  [--cwd DIR] [--model M] [--effort E] [--service-tier T] \
  [--resume ID] [--image FILE]... \
  [--timeout 10m] [--json] [--record FILE]
```

不做 `ops`、`login`、`orient`、`mcp`；这些留下一个里程碑。

## 结构

- `cmd/oarlock/main.go`：只做 `os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))`。
- `internal/cli`：可测试的命令实现、DTO、渲染、退出码、`Run(args, stdout, stderr) int`。
- `cmd/oarlock` 必须 blank import 全部 adapter（pi、claude、codex、cursor、acp），`list` 才能看到完整注册表。
- 只用标准库 `flag`；**不加第三方依赖**。

## 命令语义

### `list`
- 人类默认表：id + session/probe/models/install/login/usage/upgrade/inventory。
- `--json`：稳定 DTO，`capabilities` 为布尔对象。
- 退出 0。

### `probe <runtime|all>`
- `all`/省略 = 全部；每个 runtime 并行 probe（goroutine + 保序）。
- 一个 runtime 报错不隐藏其他；结果里带 `error`，整体退出 1。
- `installation.available=false` 不是错误，退出 0。
- 人类表 + `--json` DTO。

### `models <runtime|all>`
- 同样 per-runtime 隔离；`ErrRuntimeNotFound`、未实现 `Models`、`ListModels` 返回 typed error 分别表达为 `unsupported`/`error`，不伪造空列表。
- 人类表：runtime、model id、name、effort levels、service tiers。
- `--json` DTO。

### `run <runtime> <prompt>`
- `--cwd` 默认当前目录，解析为绝对路径；传给 `Options.Cwd`。
- `--model/--effort/--service-tier/--resume` 传给 `Options`。
- `--image` 可重复；为支持它，在根包给 `PromptAndWait` 增加一个 `WithPromptOptions(PromptOptions)`（或等价、不破坏现有 API 的方式），并加测试。图片路径解析为绝对路径。
- `--timeout` 传给 `WithTimeout`；超时是 `RunInterrupted`，退出 1（不是 130）。
- 第一个 SIGINT：取消 ctx，让 `PromptAndWait` abort；若最终是 signal interrupted，退出 130。
- 第二个 SIGINT：立即 `Dispose` 后退出 130。
- 人类模式：
  - 开始打印 `[runtime <id>]`（以及能读到的 model/effort）。
  - 通过 `Session.Events` 打印基本进度：assistant text、`[thinking]`、tool start/end、turn outcome、compaction/retry。
  - 最后打印 outcome + text。
- `--json`：不打印进度，只输出**一个**最终 DTO；运行前错误也输出带 `error` 的 DTO 并以 1 退出。
- `--record`：
  - 运行前检查路径；已存在则直接失败，不启动 session。
  - 正常完成后（含 dispose 记录）用 `internal/voyage.Create` 写 header + 全部 records + end；`O_EXCL` 不覆盖。
  - header：runtime、requested model/effort/serviceTier、cwd、sessionId、startedAt、`recorder:"oarlock-cli/<version>"`。
  - 记录在正常完成后落盘；进程崩溃时可能没有完整文件，文档要写明（live 增量写入可作加分项，不作硬性要求）。
- 退出码：
  - 0 completed
  - 1 failed / aborted / rejected / 运行前错误 / record 写入失败
  - 2 参数错误（`flag` 默认）
  - 130 第一个 SIGINT 触发的 interrupted
- `Dispose` 必须覆盖所有正常/错误路径；`os.Exit` 前先 dispose。

## DTO（`schema: "oarlock.cli/v1"`）

所有 `--json` 顶层都带 `schema` 和 `command`。示意：

```json
{"schema":"oarlock.cli/v1","command":"run","runtime":"pi","sessionId":"...","outcome":"completed","text":"...","turn":{"kind":"completed"},"status":{"kind":"idle"},"usage":{"input":1,"output":2},"context":{"tokens":3,"window":1000000},"model":"...","effort":"low","serviceTier":"default","requestId":"...","seq":42}
```

- `probe.results[]`: `runtime`, `installation` 或 `error`。
- `models.results[]`: `runtime`, `installation`, `models[]`, `unsupported` 或 `error`。
- `list.runtimes[]`: `id`, `capabilities{}`。
- 全部 DTO 定义在 `internal/cli`，带 json tag；不要直接 marshal 根包 struct。
- 写 `docs/cli.md` + `docs/cli.zh.md`，并链接到 README 与 docs 索引。

## 验收标准

- [ ] `go run ./cmd/oarlock list` / `probe pi` / `models pi` 可用（无 live 也能 probe）
- [ ] `run --cwd` 生效；`--image` 有测试
- [ ] `--json` 输出合法、稳定的 DTO；错误也有 DTO
- [ ] `--record` 已存在路径时不启动 session 并失败；新路径写出完整 voyage（header/records/end）
- [ ] 第一个/第二个 SIGINT 语义有测试或 fake 测试
- [ ] per-runtime 失败隔离 + 保序
- [ ] 无第三方依赖；gofmt/vet/test -race/Windows/darwin build 全绿
- [ ] 文档中英文入口更新

## 非目标

- ops/login/orient/mcp
- 交互式 TUI
- 读取 OAR TypeScript 的 voyage 日志
- 覆盖已有 record 文件
