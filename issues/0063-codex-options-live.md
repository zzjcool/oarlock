# Codex M6 选项面 live：第三方 endpoint 验证 exclude / resume effort / MCP

**优先级** P1 · **区域** adapter/codex · **类型** test · **里程碑** M7

## 背景

#54 的 `mcpServers`、`disallowedTools`、`userConfiguration=exclude`、resume effort 全部只有 fake app-server 证据；本机 Codex 0.165.0 未登录，但第三方 OpenAI Responses endpoint 可用（#20 已用 `CODEX_HOME` 自定义 provider 跑通 `TestLiveCodex`）。本 issue 用同一方式把 #54 的关键行为升级为 live。

## 目标

1. 在 `OARLOCK_LIVE_CODEX=1` 下增加 opt-in `TestLiveCodexOptions`：
   - 临时 `CODEX_HOME`，自定义 `model_providers` 指向 any-endpoint（base URL + `wire_api=responses`）；
   - `userConfiguration=exclude` 打开会话并完成一次真实 turn，证明隔离 home 保留登录/可用；
   - resume 后 `effort` 通过 `thread/settings/update` 设置并读回一致；
   - 至少验证 MCP server config 被 app-server 接受（能起本地 MCP echo server 更好；做不到则明确未验证）；
   - `disallowedTools` 能触发本地 MCP tool 时验证禁用，否则明确未验证。
2. token 只从环境读取（本机 `/root/.pi/agent/auth.json` 的 any-endpoint.access），不得打印、写入仓库/fixture/日志。
3. 更新 `docs/runtimes/codex.md` 与 compat-matrix 的 Codex 行：把已 live 的项标成 live，其余保持 UNVERIFIED；不要过度声明。
4. 只改 adapters/codex + 该 runtime 文档/矩阵行；不加第三方依赖。

## 验收标准

- [ ] `OARLOCK_LIVE_CODEX=1` 时 exclude + resume effort live PASS（至少这两项）
- [ ] MCP/disallowedTools 有 live 证据或明确未验证原因
- [ ] 没有 token 进入仓库/记录/日志
- [ ] 现有 fake/离线测试不退化
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 官方 Codex 账号 live
- AccountUsage
- ACP 其他 runtime

## 参考

- `adapters/codex/{live_test.go,options_test.go,home.go,mcp.go,session.go}`
- `docs/live-testing.md` 的 Codex provider 配置
- issue #20、#54

## 依赖

#20、#54（均已合入）
