# 命令行

[English](cli.md) · **中文**

`oarlock` 是命令行，包在与宿主程序相同的公共 API 之上。八个命令：

| 命令 | 回答什么问题 |
|---|---|
| `oarlock list` | 注册了哪些运行时，各自声明了什么能力？ |
| `oarlock probe [<runtime>\|all]` | vendor CLI 装了吗，版本是多少？ |
| `oarlock models [<runtime>\|all]` | 这个运行时现在能跑哪些模型？ |
| `oarlock status [<runtime>\|all]` | 这台机器登录了吗，登录的是哪个账号？ |
| `oarlock usage [<runtime>\|all]` | 账号配额现在用掉多少？ |
| `oarlock update [<runtime>\|all] --check` | 有新版本吗？（只检查，从不升级） |
| `oarlock inventory [<runtime>\|all]` | 这个运行时提供哪些 models / skills / commands / tools？ |
| `oarlock run <runtime> <prompt>` | 跑一个 turn，给出结果，可选录制 |

还没有的：`login`、`orient`、`mcp`，以及所有会改动机器的 `ops` 动作
（`install`、`upgrade`、`logout`）；四个只读 op —— `status`、`usage`、
`update --check`、`inventory` —— 已经有了。没有 TUI：人类可读的输出就是普通
表格和逐行事件转录，每个命令都有给脚本用的 `--json` 文档。

## 1. 构建与运行

```sh
go build ./cmd/oarlock        # ./oarlock（Windows 上是 oarlock.exe）
go run ./cmd/oarlock list     # 同样的东西，不留二进制
```

`cmd/oarlock` 空导入（blank import）全部 adapter（pi、claude、codex、cursor 和
ACP 家族），`list` 因此能看到完整注册表；只导入一个 adapter 的宿主程序只能看到
一个运行时。CLI 只用标准库——解析用 `flag`，没有第三方依赖。

命令的形态：

```text
oarlock list [--json]
oarlock probe [<runtime>|all] [--json]
oarlock models [<runtime>|all] [--json]
oarlock status [<runtime>|all] [--json]
oarlock usage [<runtime>|all] [--json]
oarlock update [<runtime>|all] --check [--json]
oarlock inventory [<runtime>|all] [--json]
oarlock run <runtime> <prompt> [flags]
oarlock help
```

`help`（也接受 `-h`）把用法打印到 stdout 并以 0 退出。无命令、未知命令或错误
flag 把诊断打印到 stderr 并以 2 退出。`run` 的 flag 可以放在两个位置参数之前、
之间或之后（`oarlock run pi 'fix it' --cwd /repo`）；单独的 `--` 结束 flag 解析，
因此本身以 `-` 开头的 prompt 必须放在 `--` 之后（`oarlock run pi -- '-hello'`）。

## 2. `list`

每个已注册运行时一行：id 与各可选操作的 yes/no。`yes` 表示运行时**实现了**该
操作，不表示当前账号有权使用——oarlock 从不声称账号级能力。表格节选三行：

```text
RUNTIME      SESSION  PROBE  MODELS  INSTALL  LOGIN  USAGE  UPGRADE  INVENTORY
claude       yes      yes    no      yes      yes    yes    yes      yes
cursor       yes      yes    yes     no       yes    no     yes      yes
pi           yes      yes    yes     no       no     no     no       yes
```

退出 0。注册表列出一个无法解析的 id，说明注册表本身坏了，不是运行时能力问题：
该 id 报到 stderr，命令以 1 退出。

`--json`：

```json
{"schema":"oarlock.cli/v1","command":"list","runtimes":[{"id":"pi","capabilities":{"session":true,"probe":true,"models":true,"install":false,"login":false,"usage":false,"upgrade":false,"inventory":true}}]}
```

`capabilities` 永远带齐八个布尔值；`session` 对每个已注册运行时都是 true。

## 3. `probe [<runtime>|all]`

不传参数和 `all` 都表示全部已注册运行时。每个运行时在自己的 goroutine 里探测，
结果按注册表顺序排列（注册表返回的 id 已排序），因此并发不会打乱表格或 JSON
数组。

一个运行时失败不会掩盖其他运行时的答案。探测错误只落在它自己的结果里（`error`，
加上探测能读到的 `installation`），其他结果照常打印，整条命令以 1 退出。未知
运行时 id 就是这种错误：注册表里没有这个运行时。

`installation.available: false` **不是**错误：探测跑过了，答案是"没安装"，退出 0。
探测错误——版本闸门拒绝、可执行文件损坏——才是这条命令唯一以 1 退出的原因。完全
没有声明 `Probe` 的运行时报的也是 `available: false`：含义是"未验证"（零值
`Installation`），不是编造出来的版本。

`ERROR` 列始终存在（探测成功时为空）。表格节选两行：

```text
RUNTIME      AVAILABLE  VERSION  PATH              SOURCE  ERROR
antigravity  no         -        -                 -       -
pi           yes        1.4.0    /usr/local/bin/pi path    -
```

`--json`：

```json
{"schema":"oarlock.cli/v1","command":"probe","results":[{"runtime":"antigravity","installation":{"available":false}},{"runtime":"pi","installation":{"available":true,"version":"1.4.0","path":"/usr/local/bin/pi","source":"path"}}]}
```

只有探测真的读到了 `installation` 时才同时带上它（版本闸门拒绝会带
`available` 和 `version`），再加上 `error`：
`{"runtime":"pi","installation":{…},"error":"probing pi: …"}`。什么都没读到的
失败——以及运行时 id 无法解析——没有 `installation` 字段。

## 4. `models [<runtime>|all]`

参数、并发与保序规则同 `probe`。每个结果恰好是三种诚实答案之一，绝不编造空列表：

| 结果 | 含义 | 退出码 |
|---|---|---|
| `models` | 运行时原样报告的模型 | 0 |
| `unsupported` | 这个运行时没有模型列举：未注册、未安装、`Models` 未实现，或 `ListModels` 返回 `*oarlock.UnsupportedOperation` | 0 |
| `error` | 探测或 `ListModels` 因其他原因失败 | 1 |

运行时返回空列表且无错误时结果就是空：人类表格显示 `(no models)`，JSON 结果里
既没有 `models` 也没有 `unsupported`。那是运行时自己的答案，不是占位符。

表格节选：

```text
RUNTIME  MODEL                         NAME                          EFFORT  SERVICE TIERS
kimi     (unsupported: runtime is not installed)
pi       workbuddy/deepseek-v4.1-flash workbuddy/deepseek-v4.1-flash -       -
```

`--json`：

```json
{"schema":"oarlock.cli/v1","command":"models","results":[{"runtime":"kimi","installation":{"available":false},"unsupported":"runtime is not installed"},{"runtime":"pi","installation":{"available":true,"version":"1.4.0","path":"/usr/local/bin/pi","source":"path"},"models":[{"provider":"any-endpoint","id":"workbuddy/deepseek-v4.1-flash","name":"workbuddy/deepseek-v4.1-flash"}]}]}
```

`provider`、`name`、`effortLevels`、`serviceTiers` 只在运行时报告时出现。

## 5. `status [<runtime>|all]`

参数、并发与保序规则同 `probe`：不传参数和 `all` 都表示全部已注册运行时，
每个运行时在自己的 goroutine 里被询问，结果按注册表（已排序）顺序排列，一个
运行时失败不会掩盖另一个运行时的答案。这条命令只读一样东西 ——
`Runtime.Login.AuthStatus` —— 不会先去探测安装。

每个结果恰好是三种诚实答案之一：

| 结果 | 含义 | 退出码 |
|---|---|---|
| `auth` | 运行时原样给的答案；`authenticated: false` 表示这台机器未登录，仍是答案 | 0 |
| `unsupported` | 这个运行时没有登录面：`Login` 未实现，或 `AuthStatus` 返回 `*oarlock.UnsupportedOperation` | 0 |
| `error` | `AuthStatus` 因其他原因失败：二进制没跑起来，或输出读不懂 | 1 |

接口缺失是"没有"，不是失败：`Login == nil` 报
`unsupported: "login status is not implemented"`。未知运行时 id 是同一类答案
—— `unsupported`，与 `models` 的报法一致。用法错误（坏 flag、位置参数多于
一个）在任何运行时被询问之前以 2 退出。

表格节选：

```text
RUNTIME  AUTHENTICATED  ACCOUNT          DETAIL
claude   yes            dev@example.com  -
cursor   no             -                -
pi       (unsupported: login status is not implemented)
```

`--json`：

```json
{"schema":"oarlock.cli/v1","command":"status","results":[{"runtime":"claude","auth":{"authenticated":true,"account":"dev@example.com"}},{"runtime":"cursor","auth":{"authenticated":false}},{"runtime":"pi","unsupported":"login status is not implemented"}]}
```

只要 `AuthStatus` 有回答，`auth` 就存在，所以未登录的结果也带着它；
`account`、`detail` 只在运行时报告时出现。

## 6. `usage [<runtime>|all]`

参数、并发与保序规则同 `probe`。这条命令读
`Runtime.Usage.AccountUsage`：账号级配额，而不是 `run` 从自己的流里 fold
出来的会话 token 总量。

配额从不编造。运行时回答空的 `AccountUsage`，结果就是空的 `usage` 对象，表格
显示 `(no windows)`；不会为了填满表格编出窗口、上限或重置时间。
`Runtime.Usage == nil` 报 `unsupported: "account usage is not implemented"`。

| 结果 | 含义 | 退出码 |
|---|---|---|
| `usage` | 运行时原样报告的内容（`plan`、`windows`）；空对象表示"没有配额窗口" | 0 |
| `unsupported` | 没有账号用量面：`Usage` 未实现、`AccountUsage` 返回 `*oarlock.UnsupportedOperation`，或因账号未登录而被拒绝 | 0 |
| `error` | 读取失败：限流、超时、响应体损坏、传输错误 | 1 |

分类由库给出（`internal/ops` 的 `UsageError` kind）：`unsupported` 与
`reauth_required` 是 `models` 那种"未登录"答案，不是错误；`rate_limited`、
`timeout`、`malformed`、`transport` 是错误。未知运行时 id 按 `models` 的先例
报 `unsupported`；用法错误在任何运行时被询问之前以 2 退出。

表格节选，每个窗口一行：

```text
RUNTIME  PLAN  WINDOW  USED   LIMIT    UNIT    RESETS AT
claude   pro   5h      42.5   100      percent  -
claude   pro   weekly  41     100      percent  2025-10-30T15:00:00Z
pi       (unsupported: account usage is not implemented)
```

`--json`：

```json
{"schema":"oarlock.cli/v1","command":"usage","results":[{"runtime":"claude","usage":{"plan":"pro","windows":[{"name":"5h","used":42.5,"limit":100,"unit":"percent"},{"name":"weekly","used":41,"limit":100,"unit":"percent","resetsAt":"2025-10-30T15:00:00Z"}]}},{"runtime":"pi","unsupported":"account usage is not implemented"}]}
```

`resetsAt` 是 RFC3339 时间戳，只在 vendor 报告了重置时间时出现；`unit` 同理。
一个 window 就是 vendor 自己的百分比 —— 已上线的 reader 给的是 `used` / `limit`
100 / `unit` `percent` —— 绝不换算成 vendor 从未报告过的绝对配额。

## 7. `update [<runtime>|all] --check`

只做检查，永远如此。命令只调 `Runtime.Upgrade.CheckUpdate`，别的都不碰：
`Upgrade` —— 会改动机器的那次调用 —— CLI 永远不调，无论检查结果如何。
`--check` 必填；其余参数规则同 `probe` —— 不传和 `all` 都是全部运行时，每个
运行时一个 goroutine，按注册表顺序，失败相互隔离。

`oarlock update` 或 `oarlock update pi --check extra` 是用法错误：退出 2，
stderr 打印 `usage: oarlock update [<runtime>|all] --check [--json]`，不产生
DTO，也不询问任何运行时。

| 结果 | 含义 | 退出码 |
|---|---|---|
| `update` | 检查的答案；`available: false` 表示"已是最新"，是数据 | 0 |
| `unsupported` | 没有只检查的路径：`Upgrade` 未实现，或 `CheckUpdate` 返回 `*oarlock.UnsupportedOperation` —— claude 与 cursor 的更新是"检查加安装一步走" | 0 |
| `error` | 检查因其他原因失败：读不到已安装版本，或 vendor 自己的报告读不懂 | 1 |

因为从不调用 `Upgrade`，`update --check` 不会安装任何东西；只有"检查加安装
一步走"的运行时给出 `unsupported`，而不是去猜。未知运行时 id 按 `models` 报
`unsupported`；坏 flag 同样退出 2。

表格节选：

```text
RUNTIME  UPDATE AVAILABLE  CURRENT  LATEST
codex    yes               0.51.0   0.52.0
pi       (unsupported: update check is not implemented)
```

`current`、`latest` 只在 vendor 报告时出现。

`--json`：

```json
{"schema":"oarlock.cli/v1","command":"update","results":[{"runtime":"codex","update":{"available":true,"current":"0.51.0","latest":"0.52.0"}},{"runtime":"pi","unsupported":"update check is not implemented"}]}
```

## 8. `inventory [<runtime>|all]`

参数、并发与保序规则同 `probe`。它读 `Runtime.Inventory.Inventory`：四份原生
清单（`models`、`skills`、`commands`、`tools`），每份自带状态 —— `ok`、
`unsupported` 或 `error`。`Runtime.Inventory == nil` 报
`unsupported: "inventory is not implemented"`。

分段按运行时自己的顺序输出，从不合并：读不到的一段不会抹掉其他段；状态是
`ok` 但没有条目的段保持为空 —— 表格里只有一行 `NAME=-`，JSON 里没有 `items`
键 —— 而不是塞一个占位条目。调用同时返回分段和错误时，分段照样保留（和
`probe` 保留已读到的 installation 一样），结果带上 `error`。

| 结果 | 含义 | 退出码 |
|---|---|---|
| `sections` | 运行时原样、按自己顺序给出的分段；每段保留自己的状态 | 0（某段报 `error` 时为 1） |
| `unsupported` | `Inventory` 未实现、调用返回 `*oarlock.UnsupportedOperation`，或运行时 id 未知（同 `models`） | 0 |
| `error` | 调用失败，**或**任一分段报 `status: "error"` —— 运行时自己标记失败的那段就是一次失败的读取，命令退出 1，同时其他分段照常打印 | 1 |

用法错误（坏 flag、位置参数多于一个）在任何运行时被询问之前以 2 退出。

表格节选：

```text
RUNTIME  SECTION  STATUS       NAME                          SOURCE   REASON
claude   models   unsupported  -                             -        this adapter does not read Claude's native model inventory yet
pi       models   ok           workbuddy/deepseek-v4.1-flash runtime  -
pi       skills   ok           -                             -        -
pi       tools    unsupported  -                             -        Pi 1.4.0 does not expose its tool/MCP inventory over RPC without a session
```

每个条目一行；没有条目的段打一行 `NAME=-`（运行时给了 `REASON` 就仍然显示）。

`--json`：

```json
{"schema":"oarlock.cli/v1","command":"inventory","results":[{"runtime":"claude","sections":[{"kind":"models","status":"unsupported","reason":"this adapter does not read Claude's native model inventory yet"}]},{"runtime":"pi","sections":[{"kind":"models","status":"ok","items":[{"name":"workbuddy/deepseek-v4.1-flash","source":"runtime"}]},{"kind":"skills","status":"ok"},{"kind":"tools","status":"unsupported","reason":"Pi 1.4.0 does not expose its tool/MCP inventory over RPC without a session"}]}]}
```

`kind` 与 `status` 是运行时自己的词；`items` 只在段内条目时出现，`reason` 只在
运行时给出时出现。

## 9. `run <runtime> <prompt>`

打开一个会话、发送一个 prompt、等待运行时自己的 turn end、报告结果并 dispose。
在此之前的所有检查都是运行前检查；运行前失败不打开会话、不启动运行时。

### 9.1 flag

| flag | 含义 |
|---|---|
| `--cwd DIR` | 运行时工作的目录。默认进程当前目录。解析为绝对路径；必须是已存在的目录。 |
| `--model NAME` | 运行时原生模型 id。 |
| `--effort LEVEL` | 运行时原生推理等级。 |
| `--service-tier TIER` | 运行时原生 service tier。 |
| `--resume ID` | 重新打开一个原生会话。 |
| `--image FILE` | 附加图片。可重复。相对**进程**当前目录解析为绝对路径，而不是相对 `--cwd`：shell 参数相对的是敲命令的 shell。 |
| `--timeout DURATION` | 运行时 turn end 的截止时间。默认 `10m`；`0` 表示不设截止；负数是用法错误（退出 2）。 |
| `--json` | 输出一个 JSON DTO，替代进度与摘要。 |
| `--record FILE` | 运行结束后写 voyage 日志。从不覆盖。 |

`--cwd`、`--model`、`--effort`、`--service-tier`、`--resume` 通过
`oarlock.Options` 传递；运行时自己的 `CheckOptions` 会在会话启动前拒绝已声明的
不支持组合。`--image` 路径通过 `PromptOptions.InputOptions.Images`
（`WithPromptOptions`）传给运行时。

打开会话前会先探测安装（运行时实现了 `Probe` 时）。探测错误，或运行时未安装，
都是运行前错误（退出 1），不是失败的 turn。

### 9.2 退出码

| 退出码 | 含义 |
|---|---|
| 0 | turn 完成（`outcome: completed`） |
| 1 | failed / aborted / rejected / 被 `--timeout` 中断 / 运行前错误 / record 写入失败 |
| 2 | 用法错误：坏 flag、位置参数个数不对、未知命令 |
| 130 | 第一个 SIGINT 中断了运行，且没有真实的完成抢先 |

用法错误（2）不产生 DTO：它在命令决定如何报告之前就退出了。

### 9.3 信号

第一个 SIGINT 取消 prompt context：`PromptAndWait` abort 这个 turn，在有界宽限
期内等待运行时自己的结束，报告 `interrupted`；随后会话被 dispose，命令以 130
退出。如果真实的完成抢先于信号，则算完成，退出 0。

第二个 SIGINT 不再等待：dispose 会话（最多等两秒）并以 130 退出。这条强制路径
**不**写 `--record` 日志；第一个 SIGINT 的路径会写，`end.reason` 为
`interrupted`。

`--timeout` 到期同样是 `interrupted`，但没有信号发生，所以退出 1。JSON DTO 能
区分两者：被中断的运行带 `error`（被取消的 context），超时的 DTO 没有。

### 9.4 人类可读输出

第一行给出运行时、会话目录，以及流里已经报告的 model/effort（没有就用命令行请求
的值）。之后进度行来自 `Session.Events`，每个事件一行，且只针对根 agent——子
agent 的文本不属于这段转录。

```text
[runtime pi] cwd=/repo model=claude-sonnet-4-5 effort=low
[thinking] reading internal/kernel
[text] The failing test is TestParseOdd...
[tool] bash started
[tool] bash ok
[turn] completed
```

前缀包括 `[text]`（流式 assistant 文本）、`[thinking]`（运行时脱敏时显示
`(redacted)`）、`[tool] <name> started` / `[tool] <name> <ok|failed>`（运行时
没报告结果时为 `ended`）、`[turn] <kind>`、
`[compaction] started (<trigger>)` / `[compaction] <result>`、
`[retry] attempt <n>/<max>: <reason>`。

turn 结束后打印一个摘要块：

```text
outcome: completed
turn: completed
text: The failing test is TestParseOdd...
session: 01J...
model: claude-sonnet-4-5
effort: low
usage: input=1200 output=340
status: idle
```

prompt 被拒绝时出现 `rejected: <code>` 及其原因，出错时出现 `error:`。摘要会
重复上面流式打印过的答案，所以终端记录和日志以同一段文本结束。

### 9.5 `--json`

进度关闭。stdout 恰好一行：最终的 DTO，在 turn 结束并 dispose 之后打印。运行前
错误同样产生 DTO（带 `error`）并以 1 退出，因此脚本可以无条件解析 stdout。

```json
{"schema":"oarlock.cli/v1","command":"run","runtime":"pi","sessionId":"01J...","outcome":"completed","text":"The failing test is TestParseOdd...","turn":{"kind":"completed"},"status":{"kind":"idle"},"usage":{"input":1200,"output":340},"context":{"tokens":8421,"window":1000000},"model":"claude-sonnet-4-5","effort":"low","serviceTier":"default","requestId":"req-1","seq":42}
```

字段在流里从未出现时省略：未知的 usage 数字和 context 数值保持缺失，而不是变成
猜测的 0。`model`、`effort`、`serviceTier` 报告运行时从流里 fold 出的值，缺失时
回落到请求值。`status` 在 dispose 之前 fold，所以已完成的 turn 在这里读作
`idle`，而 record 文件仍然保留包括 dispose 在内的全部 record。

运行前失败也是一个 DTO：

```json
{"schema":"oarlock.cli/v1","command":"run","runtime":"pi","error":"record file already exists: /tmp/run.voyage (refusing to overwrite)"}
```

第二个 SIGINT 的强制退出会产生一个不是由 turn 折叠出来的 DTO：
`{"schema":"oarlock.cli/v1","command":"run","runtime":"pi","outcome":"interrupted","error":"interrupted by second SIGINT"}`，
退出 130。

### 9.6 `--record`

路径相对进程当前目录解析为绝对路径，并在会话打开**之前**用 `os.Lstat` 检查。
已存在的文件直接失败、退出 1，不启动运行时：

```text
$ oarlock run pi 'fix it' --record /tmp/run.voyage
oarlock: run: record file already exists: /tmp/run.voyage (refusing to overwrite)
```

JSON 模式下同样的失败是一个带该 `error` 的 DTO，仍然退出 1。父目录必须已存在。

turn 结束并 dispose 之后，命令用 `internal/voyage` 写日志：`Create` 以 `O_EXCL`
写 header（从不覆盖），随后按稠密 `Seq` 顺序 append 全部保留的 record（包括
dispose 那条），最后 `End` 以运行结果作 reason。写入失败就是命令失败（退出 1；
本来就是中断的运行仍为 130），因为 record 是证据。

header 字段：

| 字段 | 值 |
|---|---|
| `runtime` | 运行时 id |
| `model`、`effort`、`serviceTier` | 请求值 |
| `cwd` | 绝对化的 `--cwd` |
| `sessionId` | 流里读到的根会话 id（流里没有则为空） |
| `startedAt` | Unix epoch 毫秒 |
| `recorder` | `oarlock-cli/<version>`；本地 `go build` 记录为 `oarlock-cli/dev` |

header 记录的是请求了什么，record 记录的是运行时实际报告了什么。

没有 live 增量写入：文件在运行结束时才创建，所以运行中崩溃（或第二个 SIGINT）
根本不会留下文件；写入日志过程中崩溃则可能留下半个文件。没有 `end` 行的日志是
截断的：`voyage.Read` 与 `voyage.Decode` 返回抢救出的前缀和 `ErrTruncated`，
`voyage.Replay` 拒绝它。格式与截断规则见 [voyage.md](voyage.md)（英文）。

## 10. 测试与边界

- 目前的命令：`list`、`probe`、`models`、`status`、`usage`、`update --check`、
  `inventory`、`run`。`login`、`orient`、`mcp` 还不在这个命令里，而且这里没有
  任何东西会改动机器：`update` 只做检查，也没有 `install`、`upgrade`、
  `logout`。
- 没有 TUI：一次调用一个 turn，不支持 steer，没有交互循环。
- 只用标准库：解析用 `flag`，DTO 用 `encoding/json`；不引入第三方依赖，也不改
  `go.mod`。
- DTO 定义在 `internal/cli`，它们**就是** schema。CLI 从不 marshal 根包的
  struct，因此库里的类型可以演进，而不会悄悄改变 `--json`。
- 测试固定的内容：DTO 键集合与 `oarlock.cli/v1` schema、退出码 0/1/2/130、
  `probe`/`models` 的 per-runtime 隔离与保序、四个只读 op（`status`、`usage`、
  `update --check`、`inventory`）用 fake runtime 固定的 success / `unsupported` /
  `error` 与同样的隔离保序，以及 `run` 的 scripted 会话 —— `--cwd`、`--image`、
  各项 option、`--record` 不覆盖、`--timeout` 与第一/第二个 SIGINT。它们在
  `internal/cli/*_test.go`，不需要装真实运行时。
