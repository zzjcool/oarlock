# ACP live：用 opencode + 第三方 OpenAI-compatible endpoint 跑通一家

**优先级** P0 · **区域** adapter/acp · **类型** test · **里程碑** M5

## 背景

ACP 六家目前只有 Go fake agent 测试，没有真机。`opencode` 是 model-agnostic 的 ACP agent，支持自定义 OpenAI-compatible provider；本机 any-endpoint（OpenAI Responses）可以驱动它，不需要 vendor 账号。目标不是六家全 live，而是先给 ACP family 一条真实端到端证据。

## 目标

1. 安装/定位 `opencode`（或记录精确的安装方式），在临时配置目录里配置 any-endpoint provider（base URL + env key）。
2. 用 `OAR_ACP_LIVE=1` gated test 跑：
   - `initialize` / 能力协商
   - `session/new`
   - `session/prompt` → `session/update` 流 → 运行时自己的结束
   - 至少一个 `session/cancel` 或权限请求路径（能触发就测，不能则标记未验证）
3. 断言 root 记录流 dense、turn outcome 正确、Text 非空、Status idle；Dispose 幂等。
4. 文档：`docs/runtimes/opencode.md` 或 `docs/live-testing.md` 记录 provider 配置；其他五家仍明确 UNVERIFIED。
5. 不把 token 写入仓库、fixture 或日志。

## 验收标准

- [ ] `OAR_ACP_LIVE=1` 时 opencode 真实会话 PASS（无该二进制/环境时 skip 并写清原因）
- [ ] prompt → 运行时结束的事件链完整，不合成 turn end
- [ ] cancel 或权限路径至少一条有真实证据；做不到的明确标未验证
- [ ] 其他五家 ACP profile 的 attribution/capability 声明不被夸大
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- grok/kimi/qwen/qoder/antigravity 的 vendor live
- ACP install/login/account usage

## 参考

- `adapters/acp/{open.go,session.go,project.go,profile_opencode.go}`
- `docs/runtimes/`、`docs/ops.md`
- OAR `shared/acp/*`

## 依赖

#21（已合入）
