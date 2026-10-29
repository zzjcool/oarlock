# Pi 输入面：images + Withdrawer

**优先级** P1 · **区域** adapter/pi · **类型** feature · **里程碑** M6

## 背景

Pi RPC 原生接受 image parts；当前 Go adapter 把 `Capabilities.Images` 设为 false，且没有 `Withdrawer`，compat-matrix 两格都是 ❌。这是 issue #10 留下的已知 gap，不影响基础对话，但影响输入面完整性。

## 目标

1. `Prompt` / `Queue` / `Steer` 支持 `InputOptions.Images`：
   - 从文件读取，按 Pi 的 native image part 发送（base64 + mimeType）；
   - Request 记录保留路径，不保留字节；
   - 空 text + 至少一张 image 合法；
   - 文件不可读/格式不支持时返回 typed rejection，不静默丢。
2. 实现 `oarlock.Withdrawer`：
   - 按 `inputID` 从 adapter-held queue 移除未发送的输入；
   - 已发送/不存在 → `not_queued`，不猜；
   - 原 queue request/response 记录不修改；
   - 取消后的输入处置权回到调用方。
3. `Capabilities.Images = true`；compat-matrix Pi `images`/`withdraw` 改为 ✅（或有证据的 🟡）。
4. 测试：fake Conn 的 image part/withdraw 精确断言；`OAR_PI_LIVE=1` 的 image prompt（若模型接受）与 withdraw live 证据；文档 `docs/runtimes/pi.md`。
5. 不加第三方依赖；不碰其他 adapter。

## 验收标准

- [ ] images 在 request/queue/steer 三条路径都有 fake 测试，且空 text+image 通过
- [ ] withdraw 成功/已发送/未知 id 三种结果正确，队列与记录不变
- [ ] live image prompt 有结论（通过或明确模型/endpoint 不支持），不伪造
- [ ] compat-matrix 与 `Capabilities()` 和代码一致
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 图片转码/缩放
- 其他 runtime 的 images/withdraw
- 修改 issue #1–#23 的冻结语义

## 参考

- `adapters/pi/{session.go,conn.go,session_test.go}`
- Pi RPC image part（真实 1.4.0）
- OAR `runtimes/pi/rpc/session.ts` 的 images/held queue
- issue #10、#45

## 依赖

#10、#45（均已合入）
