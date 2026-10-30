# Pi retry live：本地故障 endpoint 触发 auto_retry

**优先级** P1 · **区域** adapter/pi / test · **类型** test · **里程碑** M11

## 背景

Pi 适配器已映射 `auto_retry_start` / `summarization_retry_scheduled` → `retry` 事件，但没有任何 live fixture：需要 provider 失败才会出现。用本地故障代理（第一次返回 429/500，随后转发到 any-endpoint）可以在不碰账号的情况下触发。

## 目标

1. 起一个本地 OpenAI-compatible 代理：前 N 次请求返回 429/500（可配置），之后转发到 any-endpoint；**不改用户全局配置**，用临时 agent dir/provider 配置或等价隔离方式让 Pi 指向它。
2. 真实 Pi 1.4.0 跑一个 prompt，录到 `auto_retry_start`（及/或 `summarization_retry_scheduled`）原生帧；写 voyage fixture。
3. 离线 replay 断言 `retry{attempt,maxAttempts,delayMs,reason}` 事件、顺序、attempt 递增；Status/Usage 不被 retry 事件破坏。
4. 如果 Pi 对该失败不重试/配置无法隔离/endpoint 形状不兼容：明确 `UNVERIFIED`，附原始请求/响应/帧；不伪造 retry fixture。
5. 只改 internal/voyage、testdata/voyage、docs/voyage.md、adapters/pi 测试；`OAR_PI_LIVE=1` gated；默认离线。

## 验收标准

- [ ] 本地故障代理可重复触发 Pi retry（或明确不可行原因）
- [ ] 有 retry live fixture + 离线 replay 断言
- [ ] 不改用户全局 Pi 配置
- [ ] 不伪造 retry 事件
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 修改 Pi retry 策略
- 其他 runtime 的 retry

## 参考

- `adapters/pi/project.go` 的 retry 映射
- `docs/voyage.md` retry UNVERIFIED 段
- Pi `auto_retry_start` / `summarization_retry_scheduled` 原生形状

## 依赖

#71（已合入）
