# 运维能力：安装 / 登录 / 配额 / 升级 / 清单

**优先级** P3 · **区域** ops · **类型** feature · **里程碑** M4

## 背景

这是 OAR 所说的"脏活"：安装、检测、登录、配额、升级。**错了改就行**，不影响数据模型，所以放在最后。但没有它，"开箱即用"就差一截：用户得自己 `claude login`、自己 `npm i -g`。

## 目标

按 runtime 逐项实现，每项都遵守同一套形状：

1. `InstallPlan`（只读：会跑什么命令、出处在哪）+ `Install`（显式调用才改机器）。
2. `Login` / `Logout` / `AuthStatus`：走各家自己的登录流程，URL/设备码转出，token 不经过 oarlock。
3. `AccountUsage`：计划 + 配额窗口 + 重置时间。
4. `CheckUpdate` / `Upgrade`：用各家自己的 updater，升级后用版本再判定。
5. 全部：`UnsupportedOption`/`Unsupported` 而不是假装成功；部分失败保留各自状态。

## 验收标准

- [ ] 每个能力有 "做 / 不做" 的明确矩阵（按 runtime）
- [ ] 登录：非交互环境下能取到 URL 或设备码（不阻塞等 stdin）
- [ ] 升级：版本未变化 → 明确失败，不是 success
- [ ] 失败不污染本地配置（幂等、可重试）
- [ ] 文档：哪些命令会改机器，必须显式 opt-in

## 非目标

- 自建镜像分发
- 批量机器管理

## 参考

- OAR `docs/spec/{install,login,account-usage,update,inventory}.md`
- `runtimes/*/login.ts`

## 依赖

#19、#20、#21、#22（每个 runtime 的会话先稳定）
