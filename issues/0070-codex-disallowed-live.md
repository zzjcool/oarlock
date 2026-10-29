# Codex disallowedTools live + MCP resume 读回

**优先级** P1 · **区域** adapter/codex · **类型** test · **里程碑** M8

## 背景

#63 已用第三方 OpenAI Responses endpoint 把 Codex `userConfiguration=exclude`、resume effort 和 MCP server 配置跑成 live；但 MCP tool 调用被 approval policy 拦住，`disallowedTools` 只有 fake 证据；resume 后也没有读回 MCP server 是否仍在。需要补一次真实验证，或明确写出不能验证的原因。

## 目标

1. 复用 #63 的临时 `CODEX_HOME` + 第三方 provider；起本地 MCP echo server。
2. 调整 codex approval policy（在 headless 安全范围内）让一次 MCP tool 调用真正执行；随后设置 `disallowedTools` 指向该 tool，证明调用被原生 deny，而不是被 approval 拦。
3. resume 后读回 MCP server（`config/read`、server status 或等价原生证据），确认 resume override 没有丢掉 server；读不到就明确写未验证。
4. 加 `OARLOCK_LIVE_CODEX=1` gated 测试；更新 `docs/runtimes/codex.md`/compat-matrix；token 只从 env，禁止入仓/打印。
5. 只改 adapters/codex + 该 runtime 文档/矩阵行；不加第三方依赖。

## 验收标准

- [ ] disallowedTools live：tool 调用被 deny，且与 approval 拒绝可区分
- [ ] resume 后 MCP server 读回成功，或明确未验证原因
- [ ] 没有 token 进入仓库/记录/日志
- [ ] 现有 fake/live 测试不退化
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 官方 Codex 账号
- MCP 协议扩展
- AccountUsage

## 参考

- `adapters/codex/{live_test.go,session.go,mcp.go,options_test.go}`
- issue #54、#63
- `docs/live-testing.md` 的 Codex provider 配置

## 依赖

#54、#63（均已合入）
