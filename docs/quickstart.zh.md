# 快速开始

[English](quickstart.md) · **中文**

**目标：** 写一个宿主程序，打开一个 Agent 会话，发送一次 prompt，读回答案和
运行时的 status/usage/context/model/effort，然后 dispose 并以 0 退出。

本页所有标识符都存在于当前代码树。完整英文版（含输出示例与全部排错项）见
[quickstart.md](quickstart.md)。

## 1. 60 秒跑起来

```sh
go run ./examples/quickstart                        # 离线：只探测
OAR_QUICKSTART_LIVE=1 go run ./examples/quickstart   # 一次真实 pi turn
```

第一条命令**不打开会话、不启动 Agent 子进程、不访问网络**：注册 pi，运行只读的
`pi --version` 探测，打印结果，然后说明如何打开 live。

第二条命令在临时目录打开真实会话，发送一个 prompt，等待 Pi 自己的 turn end，
打印 fold 后的 readback，并 dispose。

环境不满足时（PATH 上没有 pi、未登录），程序打印 `skipped: <原因>` 并以 0
退出；它不会伪造成功，也不会 panic。真正的失败写到 stderr，退出码为 1。

## 2. 最小程序

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/zzjcool/oarlock"
	_ "github.com/zzjcool/oarlock/adapters/pi" // init 注册 "pi"
	"github.com/zzjcool/oarlock/observe"
)

func main() {
	ctx := context.Background()

	// 1. 通过 side effect 注册。没有上面的 blank import 会得到
	//    oarlock.ErrRuntimeNotFound。
	rt, err := oarlock.Require("pi")
	if err != nil {
		panic(err)
	}

	// 2. 只读探测：PATH 上的 pi --version，不访问账号、不打开会话、不联网。
	inst, probeErr := rt.Installation(ctx)
	fmt.Printf("runtime=%s available=%v version=%q path=%q\n",
		rt.ID, inst.Available, inst.Version, inst.Path)
	if !inst.Available {
		fmt.Println("skipped: PATH 上没有 pi")
		return
	}
	if probeErr != nil {
		panic(probeErr)
	}

	// 3. live gate。没有显式 opt-in，下面一行都不会执行。
	if os.Getenv("OAR_QUICKSTART_LIVE") != "1" {
		fmt.Println("skipped: 使用 OAR_QUICKSTART_LIVE=1 打开 live")
		return
	}

	// 4. 临时工作目录：示例不读写启动它的 checkout。
	cwd, err := os.MkdirTemp("", "oarlock-quickstart-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(cwd)

	// 5. 打开会话。Model/Effort 留空表示使用运行时自己的默认值。
	sess, err := rt.Session(ctx, inst, oarlock.Options{Cwd: cwd})
	if err != nil {
		fmt.Printf("skipped: 打开会话失败（通常是没有登录）：%v\n", err)
		return
	}
	// dispose 必须熬过结束 turn 的取消。
	defer sess.Dispose(context.WithoutCancel(ctx))

	// 6. 一个 prompt，以及运行时自己的 turn end——绝不合成。
	run, err := oarlock.PromptAndWait(ctx, sess,
		"只回复一个词：ok",
		oarlock.WithTimeout(2*time.Minute))
	if err != nil {
		panic(err)
	}
	fmt.Printf("outcome=%s text=%q\n", run.Outcome, run.Text)

	// 7. Readback：一次类型断言拿到整套读回接口。
	rb, ok := sess.(observe.Readback)
	if !ok {
		panic("session 不支持 observe.Readback")
	}
	fmt.Printf("status: %s (seq %d)\n", rb.Status().Value.Kind, rb.Status().Seq)
	if u := rb.Usage().Value.Total; u != nil { // nil 表示“未上报”，不是 0
		fmt.Printf("usage: input=%d output=%d\n", u.Input, u.Output)
	}
	if c := rb.ContextUsage().Value; c != nil && c.Usage.Tokens != nil {
		fmt.Printf("context: tokens=%d\n", *c.Usage.Tokens)
	}
	fmt.Printf("model: %q effort: %q\n", rb.Model().Value, rb.Effort().Value)

	// 8. 同一批 record，也可以自己按 cursor fold。
	folded := observe.StatusOf(sess.Records(oarlock.Cursor{}))
	fmt.Printf("status(from records, seq %d)=%s\n", folded.Seq, folded.Value.Kind)
}
```

## 3. 顺序就是重点

```
注册 → 探测 → gate → 打开会话 → prompt/wait → 读回 → dispose
```

- gate 之前不打开会话。
- gate 之后不发明运行时没有报告的 outcome。
- `PromptAndWait` 只调用 `Prompt` 和 `Abort`；它不向流里追加 record。

## 4. 读回与能力发现

- `observe.Readback` 把 Status、Usage、ContextUsage、Model、Effort 聚在一个
  类型断言里；`QueryResult[T]` 带 `Seq`，可和 record 流对齐。
- 每次读回都从 record 重新 fold，返回前会 clone；不要用整 struct 的 `==`
  比较两次读回，比较 `Seq` 和关心的字段。
- 能力用 interface 表达：`oarlock.Steerer`、`oarlock.Withdrawer`、
  `oarlock.ContextBreakdownReader` 存在即可用；不存在就是做不到。没有
  `SupportsXxx() bool` 这种会漂移的声明。
- 不想用 readback，也可以直接 `observe.StatusOf(sess.Records(oarlock.Cursor{}))`。

## 5. 流保证

一个会话只有一条 append-only record 流：

- `Records(Cursor{})` 重放全部保留记录；
- `Cursor{SessionID, AfterSeq}` 只重放没见过的部分；
- `Events(cursor, fn)` 在重放后继续 live；
- 运行时报告什么就记录什么；turn end 不合成；
- `WithTimeout` 或调用方 ctx 触发时，`PromptAndWait` 返回
  `RunInterrupted`，不会伪造 `RunCompleted`。

细节见 [architecture.md](architecture.md) §1 和冻结的
[spec/record-stream.md](spec/record-stream.md)。

## 6. 这个示例不做什么

- 没有 `OAR_QUICKSTART_LIVE=1` 时不打开会话、不启动 Agent、不联网。
- 不持有 API key/token；凭证留在 vendor CLI 自己的存储里。
- 不选择 model 或 effort；空字段表示运行时默认值。
- 不做重试、队列或持久化历史；那是宿主的职责。

## 7. 常见问题

| 现象 | 原因 | 处理 |
|---|---|---|
| `runtime not registered: "pi"` | 没有 import pi adapter，`init` 未执行 | 加 `import _ "github.com/zzjcool/oarlock/adapters/pi"` |
| `available: false` + 找不到 pi | PATH 上没有可驱动的 pi | 安装 pi 1.4.0 或放进 PATH |
| `VersionError` / `ErrVersionMismatch` | Pi 适配器是精确 1.4.0 闸门 | 换用 1.4.0 二进制 |
| 打开会话失败 | 通常是未登录 | 用 pi 自己登录；其他运行时看 `rt.Login` |
| `outcome: rejected` + `busy` | 上一轮还没结束 | 等 turn end 或先 abort |
| `outcome: interrupted` | `WithTimeout` 到期，已 abort 并等待运行时自己的结束 | 这不是 vendor outcome；流里仍保留实际发生的事 |

完整排错（包括 caller-cancel、readback 比较、codex/cursor gate）见英文版
[quickstart.md §7](quickstart.md#7-troubleshooting)。
