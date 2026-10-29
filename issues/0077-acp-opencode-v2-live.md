# ACP opencode v2 live

**优先级** P1 · **区域** adapter/acp · **类型** test · **里程碑** M9

## 背景

opencode v1 已 live；v2 profile（`@opencode/cli` 2.x）的 `nested`、opaque、无 steer、无 usage 等声明仍只靠 `opencode-acp-v2-child.json` 录制。需要把 v2 装到临时前缀并跑一次真实 ACP live。

## 目标

1. 把 `@opencode/cli@2.0.26`（或可用 2.x）安装到临时前缀，不覆盖 v1；用临时 XDG provider 配置指向第三方 OpenAI-compatible endpoint（token 只经 env）。
2. 用 `OAR_ACP_LIVE=1` 的 v2 测试跑：initialize、session/new、prompt→runtime 自己的结束；验证：
   - v2 profile 的 attribution/nested 声明与真实流一致；
   - v2 不满足 `oarlock.Steerer`（编译期）；
   - usage 未上报时保持 nil，不造 0；
   - 能触发 `_meta` child session 就验证 nested，不能则明确未验证。
3. 更新 `docs/runtimes/opencode.md`、compat-matrix ACP 行；其他五家仍 UNVERIFIED。
4. 只改 adapters/acp + 文档；不加 Go 第三方依赖。

## 验收标准

- [ ] v2 live 会话 PASS（或明确环境不可用原因）
- [ ] nested/opaque/Steer/usage 的声明与真实证据一致
- [ ] v1 live 与 fake 测试不退化
- [ ] token 不入仓/日志
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 其他五家 ACP live
- ACP install/login/account usage

## 参考

- `adapters/acp/{profile_opencode.go,testdata/opencode-acp-v2-child.json,family_test.go}`
- `docs/runtimes/opencode.md`
- issue #43、#55

## 依赖

#43、#55（均已合入）
