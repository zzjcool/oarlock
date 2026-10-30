# Models 覆盖：Claude / Codex / ACP

**优先级** P1 · **区域** adapter-claude / adapter-codex / adapter-acp · **类型** feature · **里程碑** M13

## 背景

CLI 的 `oarlock models` 目前只对 Pi、Cursor 有数据；Claude/Codex/ACP 显示 `unsupported`。OAR 的 `listModels` 在这些 runtime 上已有实现，需要按行为移植，不照抄代码。

## 目标

1. 给 Claude、Codex、ACP 六家实现 `oarlock.ModelLister`：
   - 返回 `[]oarlock.Model`（Provider/ID/Name/EffortLevels/ServiceTiers）；
   - 未登录/无权限/接口不支持 → typed error 或明确的 unsupported，不返回空列表冒充成功；
   - 不新增第三方依赖；不启动无关 session。
2. 参考 OAR 的 `runtimes/{claude,codex,opencode,grok,kimi,qwen,qoder,antigravity}/list-models.ts` 与现有 adapter 的 fake 测试风格。
3. 测试：
   - fake CLI/ACP 响应覆盖成功、未登录、空模型、字段缺失；
   - 不要求 live；live 明确标 UNVERIFIED。
4. 更新 `docs/runtimes/{claude,codex,opencode}.md`、`docs/compat-matrix.md` 的 models 行；CLI `models` 自动生效，不需要改 CLI。
5. 不破坏现有 `ListModels`（Pi/Cursor）与所有 adapter 测试。

## 验收

- [ ] Claude/Codex/ACP 的 `ListModels` 有 fake 成功/失败测试
- [ ] 未登录不伪装成空列表
- [ ] CLI `oarlock models claude` 等能显示模型或明确 unsupported（不要求 live）
- [ ] docs/matrix 更新
- [ ] gofmt/vet/test -race/Windows+darwin build 全绿

## 非目标

- 真实账号 live 验证
- AccountUsage
- Cursor 的模型列表（已存在）
