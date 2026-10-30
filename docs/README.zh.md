# 文档

[English](README.md) · **中文**

想先跑起来，看快速开始；想理解契约，看架构。其余文档要么是参考，要么是某个
决策的证据。

> 目前只有入口文档提供中文；深层的 spec 与运行时指南仍是英文。中文页与英文页
> 不一致时，以英文页为准，直到译文更新。

## 从这里开始

| 文档 | 回答什么问题 |
|---|---|
| [quickstart.zh.md](quickstart.zh.md) · [English](quickstart.md) | 如何探测、打开会话、发 prompt、读回并 dispose？ |
| [cli.zh.md](cli.zh.md) · [English](cli.md) | `oarlock` 命令能做什么：list、probe、models、run？ |
| [architecture.md](architecture.md) | 分层、record 流和能力模型是什么？（英文） |
| [../examples/quickstart](../examples/quickstart) | 同样流程的可运行程序 |

## 参考

| 文档 | 回答什么问题 |
|---|---|
| [compat-matrix.md](compat-matrix.md) | 每个运行时支持什么，哪些差异是已声明的？（英文） |
| [ops.md](ops.md) | 安装、登录、登录状态、AccountUsage、更新、清单（英文） |
| [runtimes/pi.md](runtimes/pi.md) | Pi RPC、控制面、投影、resume、读回、版本（英文） |
| [runtimes/pi-projection.md](runtimes/pi-projection.md) | Pi 原生帧到 record 的详细映射（英文） |
| [runtimes/claude.md](runtimes/claude.md) | Claude stream-json、steer、images、cancel、权限（英文） |
| [runtimes/codex.md](runtimes/codex.md) | Codex app-server、MCP、选项、subagent、resume（英文） |
| [runtimes/cursor.md](runtimes/cursor.md) | Cursor CLI 路径与已声明降级（英文） |
| [runtimes/opencode.md](runtimes/opencode.md) | ACP opencode v1/v2 两条线（英文） |
| [spec/record-stream.md](spec/record-stream.md) | 冻结的 record 流契约（英文） |
| [spec/capabilities.md](spec/capabilities.md) | 能力差异如何声明（英文） |
| [spec/errors.md](spec/errors.md) | 错误与拒绝的 taxonomy（英文） |
| [spec/observers.md](spec/observers.md) | 观察者投递与背压（英文） |
| [spec/redaction.md](spec/redaction.md) | 凭证规则与保留内容（英文） |

## 证据与测试

| 文档 | 内容 |
|---|---|
| [voyage.md](voyage.md) | 录制/回放格式、live fixture 与固定事实（英文） |
| [live-testing.md](live-testing.md) | opt-in live gate、环境配置、排错（英文） |
| [benchmarks-and-fuzz.md](benchmarks-and-fuzz.md) | fold/stream-json benchmark 与脱敏 fuzz（英文） |

## 项目

| 文档 | 内容 |
|---|---|
| [board.md](board.md) | 里程碑、issue 与状态（英文） |
| [roadmap.md](roadmap.md) | 已完成内容与仍被环境阻塞的事项（英文） |
| [adr/](adr/) | 架构决策与被否决的方案（英文） |
| [../issues/](../issues/) | 已完成的 issue 归档 |

## 中文入口

- [../README.zh.md](../README.zh.md) —— 项目总览
- [README.zh.md](README.zh.md) —— 本索引
- [quickstart.zh.md](quickstart.zh.md) —— 可运行的快速开始
- [cli.zh.md](cli.zh.md) —— 命令行
