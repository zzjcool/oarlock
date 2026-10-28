# Go 能力模型：接口组合 + 运行期声明 + 拒绝

**优先级** P0 · **区域** core · **类型** spec · **里程碑** M0

## 背景

Go 没有"可选成员"。如果照搬 OAR 的 `runtime.steer?`，最自然的错误写法是 `SupportsSteer() bool`——布尔值和实现会漂移，而且编译期不强制调用方处理缺失。另一个错误是静默忽略不支持的选项（multica 的做法）。

## 目标

1. 定义 `Session` 基础接口 + 可选能力接口：`Steerer`、`Withdrawer`、`ContextBreakdownReader`。
2. 定义 `Runtime`：`Session` 必需；`Probe/Models/Install/Login/Usage/Upgrade/Inventory` 为可选字段。
3. 定义 `RefusedOptions map[Option]string` 与 `*UnsupportedOption`，在 open 阶段拒绝。
4. 定义 `Capabilities()`：`Queue.Durable`、`Attribution`、`Images`、`ContextBreakdown`。
5. 明确"声明只在行动前必须知道时存在"，其余留空。

## 验收标准

- [ ] `docs/spec/capabilities.md`，含一个"同一段宿主代码跑在两个 runtime 上"的示例
- [ ] 三段式规则（接口 / 拒绝 / 声明）各有反例说明为什么不用另外两种
- [ ] 列出所有 `Option` 常量（systemPrompt, appendSystemPrompt, env, mcpServers, mcpServerCredentials, disallowedTools, launchArgs, serviceTier, additionalDirectories, outputSchema, name, userConfiguration）
- [ ] 明确 `AttributionTier` 四值：none / opaque / attributed / nested

## 非目标

- 各 runtime 的具体取值（见 #15、#22 与 compat-matrix）
- 配置表单（forms）——不在 oarlock 范围

## 参考

- OAR `docs/design/capabilities.md`、`packages/oar/src/contracts/runtime.ts`
- 本仓 `docs/architecture.md` §2、ADR-0002

## 依赖

#1（记录流决定 `AgentPath` 的类型）
