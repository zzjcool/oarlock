# Pi 版本闸门与 refused options

**优先级** P1 · **区域** adapter/pi · **类型** feature · **里程碑** M1

## 背景

OAR 对 Pi 的版本策略是"**精确匹配**"：`pi --version` 不等于内置版本就拒绝，因为 RPC 方言随版本变化。同时 RPC 模式有一组自己的 `refusedSessionOptions`（例如 `launchArgs`，因为启动参数由库拥有）。

## 目标

1. 启动前执行 `<entry> --version`，15s 超时，失败给出可操作错误。
2. 版本不在支持窗口 → typed error（不是 warning 后继续）。
3. `RefusedOptions` 表：`launchArgs`、`systemPrompt`（如 Pi 拒绝）、`mcpServerCredentials` 等，逐项带原因。
4. 拒绝发生在**任何进程启动之前**，并且与声明同源（同一张表驱动）。
5. 版本信息进记录（`init` frame 的 `native`）。

## 验收标准

- [ ] 版本不符 → 明确错误文案，含期望/实际
- [ ] 拒绝选项测试：声明过的必然拒绝，未声明的不得拒绝（防漂移）
- [ ] `--version` 超时 → 不挂死，进程被杀
- [ ] 支持窗口写进 `docs/compat-matrix.md` 并链接本 issue

## 非目标

- 自动升级（#23）
- 其他 runtime 的窗口（各自 issue）

## 参考

- OAR `runtimes/pi/rpc/{launch,open,version}.ts`
- `docs/design/capabilities.md`

## 依赖

#9
