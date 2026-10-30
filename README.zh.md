# oarlock

[English](README.md) · **中文**

一个用 Go 驱动编码 Agent 运行时的统一契约。

> *oarlock* 是船桨的桨架。它本身不产生力量，只保证力量原样传到需要的地方。

oarlock 是 [OAR](https://github.com/botiverse/oar) 契约的 Go 实现。宿主用同一套
API 驱动十种编码 Agent：探测安装、打开会话、发送 prompt、观察一条有序且无损的
record 流、steer/abort、读取 usage 与 context，并在之后 resume。

不内嵌 vendor SDK，不用 cgo，Go module 里没有 JavaScript。

## 快速开始

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/zzjcool/oarlock"
	_ "github.com/zzjcool/oarlock/adapters/pi" // 注册 "pi"
)

func main() {
	ctx := context.Background()

	rt, err := oarlock.Require("pi")
	if err != nil {
		log.Fatal(err)
	}
	inst, err := rt.Installation(ctx) // 只读探测，不访问账号或网络
	if err != nil {
		log.Fatal(err)
	}
	sess, err := rt.Session(ctx, inst, oarlock.Options{Cwd: "/repo"})
	if err != nil {
		log.Fatal(err)
	}
	defer sess.Dispose(context.WithoutCancel(ctx))

	run, err := oarlock.PromptAndWait(ctx, sess,
		"修复这个不稳定的测试",
		oarlock.WithTimeout(10*time.Minute))
	if err != nil {
		log.Fatal(err)
	}
	log.Println(run.Outcome, run.Text) // completed | failed | aborted | interrupted
}
```

可运行版本（含 readback）在 [`examples/quickstart`](examples/quickstart)。
完整说明见 [docs/quickstart.zh.md](docs/quickstart.zh.md)（英文版
[docs/quickstart.md](docs/quickstart.md)）。

## 命令行

`cmd/oarlock` 是建立在同一套 API 之上的 Phase 1 命令行——`list`、`probe`、
`models`、`run`——输出普通表格或一份 JSON 文档，没有 TUI。

```sh
go run ./cmd/oarlock list                 # 已注册的运行时及其声明能力
go run ./cmd/oarlock probe all --json     # 装了吗？一份 JSON 文档
go run ./cmd/oarlock run pi '修复这个不稳定的测试' --cwd .
```

flag（`--cwd`、`--model`、`--effort`、`--service-tier`、`--resume`、可重复的
`--image`、`--timeout`）、退出码、`--record` 与 DTO schema 见
[docs/cli.zh.md](docs/cli.zh.md)（[English](docs/cli.md)）。

## 运行时

| 运行时 | 传输方式 | 证据 |
|---|---|---|
| **pi** | RPC 子进程（`pi --mode rpc`，JSONL） | live：turn、工具、abort、compaction、retry、steer、images、withdraw、resume |
| **claude** | `-p` + 双向 stream-json | 经第三方 endpoint live：PromptAndWait、steer、images、cancel；官方登录未验证 |
| **codex** | app-server JSON-RPC | 经第三方 endpoint live：MCP、选项、subagent、resume；官方登录未验证 |
| **grok、kimi、opencode、qwen、qoder、antigravity** | ACP over stdio | opencode v1 + v2 live；其余五家 live 未验证 |
| **cursor** | `cursor-agent -p --output-format stream-json` | live：PromptAndWait、显式 resume；有已声明的降级 |

十种运行时全部走 stdio。Pi 运行时需要 Node；Go 构建本身不需要。

## 设计概览

- **`oarlock`** —— 契约层：三态 record、稠密 `Seq`、typed error、以可选
  interface 表达的能力，以及 `PromptAndWait`。
- **`internal/kernel` + `internal/fold`** —— append-only 流、cursor 重放、
  不反压 run 的观察者，以及纯函数式的 status/usage/context/conversation fold。
- **`adapters/*`** —— Pi、Claude、Codex、ACP 家族、Cursor。每个 adapter 把
  原生帧映射为同一种 record；不包装 vendor SDK。
- **`internal/ops`** —— 安装、登录/登出、登录状态、AccountUsage、更新、清单；
  能力缺失时给出 typed 结论，不伪造成功。
- **`internal/voyage` + `testdata`** —— 录制一次，永久回放。live 录制的
  fixture 是 fold 的回归基线。

完整模型见 [docs/architecture.md](docs/architecture.md)，逐运行时差异见
[docs/compat-matrix.md](docs/compat-matrix.md)。

## 环境要求

- Go 1.23 或更新版本。
- 要驱动的 vendor CLI（Pi 运行时还需要 Node >= 22.19）。
- Go module 不依赖 cgo、Node 或 npm。

凭证始终留在 vendor CLI 自己的存储里。oarlock 不持有、不写入，并在数据进入
record 或 error 之前按规则脱敏。Node 边界见
[ADR-0005](docs/adr/0005-node-boundary.md)。

## 文档

| 从这里开始 | |
|---|---|
| [docs/README.zh.md](docs/README.zh.md) · [English](docs/README.md) | 文档索引，按主题组织 |
| [docs/quickstart.zh.md](docs/quickstart.zh.md) · [English](docs/quickstart.md) | 探测 → 会话 → prompt → readback → dispose |
| [docs/cli.zh.md](docs/cli.zh.md) · [English](docs/cli.md) | `oarlock` 命令：list、probe、models、run |
| [docs/architecture.md](docs/architecture.md) | 分层、record、能力模型（英文） |

| 参考 | |
|---|---|
| [docs/compat-matrix.md](docs/compat-matrix.md) | 运行时支持矩阵与已声明降级（英文） |
| [docs/ops.md](docs/ops.md) | 安装、登录、usage、更新、清单（英文） |
| [docs/runtimes/](docs/runtimes/) | 每个运行时的指南（英文） |

| 证据与项目 | |
|---|---|
| [docs/voyage.md](docs/voyage.md) | 录制/回放 fixture 与 live 证据（英文） |
| [docs/live-testing.md](docs/live-testing.md) | live 测试 gate 与排错（英文） |
| [docs/benchmarks-and-fuzz.md](docs/benchmarks-and-fuzz.md) | benchmark 与脱敏 fuzz（英文） |
| [docs/board.md](docs/board.md) · [docs/roadmap.md](docs/roadmap.md) | 里程碑与剩余工作（英文） |
| [docs/adr/](docs/adr/) | 架构决策记录（英文） |
| [issues/](issues/) | 已完成 issue 归档 |

## 当前状态

**M0–M12 全部完成 —— 47/47 个 issue。**

Phase 1 命令行——`list`、`probe`、`models`、`run`，带稳定的 `--json` DTO 与
不覆盖已有文件的 `--record`——在 [`cmd/oarlock`](cmd/oarlock)；说明见
[docs/cli.zh.md](docs/cli.zh.md)。

- 已有 live 证据：Pi、Cursor、Claude/Codex（经第三方 endpoint）、opencode
  的 v1 与 v2。
- 仍未验证：Claude/Codex 官方登录、真实 AccountUsage 数据、其余五家 ACP、
  Pi subagent、summarization retry、阈值/回合中 compaction。
- CI 在 ubuntu、macOS、Windows 以及 cross-build 上全绿。

逐 gate 的记录在 [docs/board.md](docs/board.md)；剩余项及原因在
[docs/roadmap.md](docs/roadmap.md)。

## 非目标

- 通用 Agent 编排框架（队列、DAG、worktree、邮箱）。那是宿主的职责。
- 存储或回放服务。oarlock 只产生 record，持久化由宿主决定。
- 浏览器运行时。
- 通过库读取 vendor 的原生会话历史。
- 做 OAR TypeScript API 的 drop-in replacement。

## 相关项目

- [botiverse/oar](https://github.com/botiverse/oar) —— 本实现遵循的契约
- [liliang-cn/agentexec](https://github.com/liliang-cn/agentexec) —— Go CLI adapter 层（MIT）
- [multica](https://github.com/multica-ai/multica) —— 生产环境 Go Agent 后端
- [coder/acp-go-sdk](https://github.com/coder/acp-go-sdk) —— ACP client

## License

Apache-2.0。见 [LICENSE](LICENSE) 与 [NOTICE](NOTICE)。
