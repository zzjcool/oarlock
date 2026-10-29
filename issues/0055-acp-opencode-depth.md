# ACP opencode 深度：usage ledger / load+resume / steer+images / profile evidence

**优先级** P1 · **区域** adapter/acp · **类型** feature · **里程碑** M6

## 背景

opencode 1.19.5 的 ACP live 已跑通 turn/cancel/permission；但 usage ledger 仍缺席，`session/load`/resume、steer、images 只是能力声明，profile 的 attribution 仍是源码推导。OAR 的 `shared/acp` 有对应处理。

## 目标

1. **usage**：把 opencode prompt answer 的 usage（若存在）写入 `Usage/Tokens/Total`；未上报保持 nil，不造 0。
2. **load/resume**：`session/load` 真实恢复一个会话，新流 Seq 从 1；失败给出 typed error。
3. **steer / images**：按 initialize 协商的能力实现或明确 unsupported；steer 用 ACP 的原生通道（`_meta.sendNow` 或 session/prompt 的并发能力），只有运行时确认才 accepted。
4. **profile 证据**：用真实 opencode 记录 attribution/capability 证据；v1/v2 的 opaque/nested 声明不得升级到未测值。
5. 更新 `docs/runtimes/opencode.md`、compat-matrix 的 ACP 行；`OAR_ACP_LIVE=1` gated 测试；其他五家仍 UNVERIFIED。
6. 不加第三方依赖；不碰其他 adapter。

## 验收标准

- [ ] usage 有值/无值两种情况都不伪造
- [ ] load/resume live 通过或明确未验证
- [ ] steer/images 只有真实确认才 accepted；否则编译期/运行期明确 unsupported
- [ ] 其他五家不被夸大
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- grok/kimi/qwen/qoder/antigravity live
- ACP install/login/account usage

## 参考

- `adapters/acp/{session.go,project.go,profile_opencode.go,runtime.go}`
- OAR `shared/acp/{session.ts,projection.ts,token-usage.ts,opening.ts}`
- issue #21、#43

## 依赖

#21、#43（均已合入）
