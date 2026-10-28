# Pi 清单：models / skills / commands / tools

**优先级** P1 · **区域** adapter/pi · **类型** feature · **里程碑** M1

## 背景

宿主需要知道"这个 runtime 现在能跑什么、有哪些技能/命令/工具"，而每家都用自己的目录与配置。OAR 把四类清单统一成 `InventoryResult<T>`，并且**部分失败保留自身状态**（不是整体报错）。

## 目标

1. `ListModels`：返回可运行模型 + effort 档位；未登录是明确答案，不是空列表。
2. `Skills` / `Commands`：读 Pi 原生目录（项目级 + 用户级），返回路径与描述。
3. `Tools`：MCP 与内置工具的现状。
4. 结果类型：`ok | unsupported | error`，每一项带来源。
5. 只读：不触发安装、不修改配置。

## 验收标准

- [ ] 本地真实环境下列出 ≥1 个模型、≥1 个 skill（如果有）
- [ ] `unsupported` 与"空列表"在类型上可区分
- [ ] 单项失败不影响其他项（错误带来源）
- [ ] 读取不产生网络请求（除非模型列表本身需要）

## 非目标

- 安装/升级（#23）
- 账号用量

## 参考

- OAR `contracts/inventory.ts`、`runtimes/pi/inventory.ts`、`runtimes/pi/rpc/readers.ts`

## 依赖

#15（版本闸门决定读法）
