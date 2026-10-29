# Board

> **Live board:** https://github.com/users/zzjcool/projects/5 (public)
>
> Milestones are the swimlanes; `priority/*` labels are the columns.
> A readable mirror of the same data follows.

| Milestone | Done | Issues | Exit gate |
|---|---|---|---|
| **M0 Spec** | 4/4 | #1, #2, #3, #4 | 记录流 / 能力 / 错误三份 spec 冻结 |
| **M1 Pi** | 12/12 | #5, #6, #7, #8, #9, #10, #11, #12, #13, #14, #15, #16 | 真实 Pi 会话可由 cursor 重放得到同一 Status/Usage |
| **M2 Kernel** | 3/3 | #17, #18, #19 | Claude 与 Pi 跑同一套行为 suite，Claude 不新增 kernel 代码 |
| **M3 Reach** | 3/3 | #20, #21, #22 | 10 家各有一份 golden trace 与能力行 |
| **M4 Ops** | 1/1 | #23 | 安装/登录/配额/升级逐项 typed 且诚实 |

## M0 Spec

| # | P | 标题 | 状态 |
|---|---|---|---|
| [#1](https://github.com/zzjcool/oarlock/issues/1) | P0 | 记录流契约：三态记录 + 稠密 seq + 归因 + cursor | ☑ Done |
| [#2](https://github.com/zzjcool/oarlock/issues/2) | P0 | Go 能力模型：接口组合 + 运行期声明 + 拒绝 | ☑ Done |
| [#3](https://github.com/zzjcool/oarlock/issues/3) | P0 | 错误与拒绝码：8 种 typed code + 两类错误 | ☑ Done |
| [#4](https://github.com/zzjcool/oarlock/issues/4) | P1 | 脱敏契约：名称规则 / URL 密码 / 结构化字段 / 错误文本 | ☑ Done |

## M1 Pi

| # | P | 标题 | 状态 |
|---|---|---|---|
| [#5](https://github.com/zzjcool/oarlock/issues/5) | P0 | Spike：Pi RPC 握手 + 一次 turn（Go） | ☑ Done |
| [#6](https://github.com/zzjcool/oarlock/issues/6) | P0 | kernel：append / observe / cursor / reachability | ☑ Done |
| [#7](https://github.com/zzjcool/oarlock/issues/7) | P0 | folds：status / usage / context / conversation | ☑ Done |
| [#8](https://github.com/zzjcool/oarlock/issues/8) | P0 | 观察者扇出与背压策略 | ☑ Done |
| [#9](https://github.com/zzjcool/oarlock/issues/9) | P0 | Pi 传输层：launch + JSONL 关联 + 超时 + 退出 | ☑ Done |
| [#10](https://github.com/zzjcool/oarlock/issues/10) | P0 | Pi session 控制面：prompt / queue / abort / dispose | ☑ Done |
| [#11](https://github.com/zzjcool/oarlock/issues/11) | P0 | Pi 投影：事件 → 统一 Event + usage / context | ☑ Done |
| [#12](https://github.com/zzjcool/oarlock/issues/12) | P1 | Pi resume 语义与拒绝证据 | ☑ Done |
| [#13](https://github.com/zzjcool/oarlock/issues/13) | P1 | Pi 状态回读：model / effort / context / mode | ☑ Done |
| [#14](https://github.com/zzjcool/oarlock/issues/14) | P1 | Pi 清单：models / skills / commands / tools | ☑ Done |
| [#15](https://github.com/zzjcool/oarlock/issues/15) | P1 | Pi 版本闸门与 refused options | ☑ Done |
| [#16](https://github.com/zzjcool/oarlock/issues/16) | P1 | 进程树 / 进程组 / Windows Job / `.cmd`→`.ps1` | ☑ Done |

## M2 Kernel

| # | P | 标题 | 状态 |
|---|---|---|---|
| [#17](https://github.com/zzjcool/oarlock/issues/17) | P1 | voyage 录制回放 + golden fixture 工装 | ☑ Done |
| [#18](https://github.com/zzjcool/oarlock/issues/18) | P1 | scripted runtime：测试替身 | ☑ Done |
| [#19](https://github.com/zzjcool/oarlock/issues/19) | P1 | Claude 适配器：stream-json（含双向 steer） | ☑ Done |

## M3 Reach

| # | P | 标题 | 状态 |
|---|---|---|---|
| [#20](https://github.com/zzjcool/oarlock/issues/20) | P2 | Codex 适配器：app-server JSON-RPC | ☑ Done |
| [#21](https://github.com/zzjcool/oarlock/issues/21) | P2 | ACP 家族：grok / kimi / opencode / qwen / qoder / antigravity | ☑ Done |
| [#22](https://github.com/zzjcool/oarlock/issues/22) | P2 | Cursor 适配器：CLI + 显式降级 | ☑ Done |

## M4 Ops

| # | P | 标题 | 状态 |
|---|---|---|---|
| [#23](https://github.com/zzjcool/oarlock/issues/23) | P3 | 运维能力：安装 / 登录 / 配额 / 升级 / 清单 | ☑ Done |

---

## Filters that act as columns

- P0 / 当前必做: `is:issue label:priority/p0`
- 按里程碑: `is:issue milestone:"M1 Pi"`
- 全部未完成: `is:issue is:open`

## Rebuilding this board from scratch

`scripts/setup-board.sh` (run once; it creates a new project, a `Priority`
field, and one item per issue). It needs a token with the `project` scope:

```bash
gh auth refresh -s project,read:project
./scripts/setup-board.sh
```
