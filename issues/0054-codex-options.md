# Codex 选项面：MCP / disallowedTools / userConfiguration / resume effort

**优先级** P1 · **区域** adapter/codex · **类型** feature · **里程碑** M6

## 背景

Codex adapter 的 app-server 会话/steer/queue/审批/子 thread 已实现（live 未验证，无官方登录）；但 `mcpServers`、`mcpServerCredentials`、`disallowedTools`、`userConfiguration=exclude` 目前直接拒绝，resume 后 effort 也拒绝。OAR 的实现通过 `thread/start`/`thread/resume` 的 config overrides 和 `thread/settings/update` 完成。

## 目标

1. 实现 `mcpServers` / `mcpServerCredentials`：按 app-server 的 config override 传递；stdin/HTTP/env/headers 的脱敏与记录规则保持不变。
2. 实现 `disallowedTools`：通过 codex 原生 deny/config 通道；不做 oarlock 侧过滤。
3. 实现 `userConfiguration=exclude`：不加载用户配置但保留登录/会话。
4. resume 后 `effort`：用 `thread/settings/update` 或等价原生通道，并读回确认；不能确认就拒绝。
5. 更新 `Refused()`、compat-matrix、`docs/runtimes/codex.md`；fake app-server 测试（成功/拒绝/字段透传/脱敏）。
6. 不加第三方依赖；不碰其他 adapter；无 codex 登录时 live 全 skip 并标注。

## 验收标准

- [ ] 四个选项/能力有 fake 测试，声明与检查同源
- [ ] 拒绝的选项不再出现在 `Refused()`，接受的选项不得静默丢
- [ ] resume effort 读回不一致时明确报错
- [ ] gofmt/vet/test -race/Windows build 全绿
- [ ] 矩阵与文档不再过度声明

## 非目标

- Codex cloud/remote
- AccountUsage
- 官方账号 live

## 参考

- `adapters/codex/{open.go,session.go,rpc.go,ops.go}`
- OAR `runtimes/codex/{open.ts,session.ts,tool-denials.ts,options.ts}`
- issue #20

## 依赖

#20（已合入）
