# scripted runtime：测试替身

**优先级** P1 · **区域** test · **类型** feature · **里程碑** M2

## 背景

OAR 提供 `@botiverse/oar/testing` 的 scripted runtime：有真实 `Session` 语义（busy、队列、abort、turn end），但不需要二进制、不需要登录。没有它，宿主只能用真模型写测试，慢且不稳定；有了它，kernel/折叠加宿主逻辑都能在毫秒级验证。

## 目标

1. `scripted.New(script...)`：按脚本产生事件、延迟、失败。
2. 复用**真实 kernel**，不绕过 seq/可达性/控制记录。
3. 可注入故障：进程中途死亡、turn 不结束、abort 不响应、未知事件。
4. 与真 adapter 共享同一套行为测试（同一份 suite 跑两边）。

## 验收标准

- [ ] 行为 suite 能在 scripted 与真实 Pi 上同时通过
- [ ] 故障注入：`exited` 后所有控制被拒
- [ ] 无 goroutine 泄漏（go test -race + goleak）
- [ ] 文档示例：宿主如何用它写自己的测试

## 非目标

- 模拟真实模型输出质量
- 网络层模拟

## 参考

- OAR `packages/oar/src/testing/scripted-runtime.ts`

## 依赖

#6、#7
