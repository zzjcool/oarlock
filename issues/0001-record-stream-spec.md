# 记录流契约：三态记录 + 稠密 seq + 归因 + cursor

**优先级** P0 · **区域** core · **类型** spec · **里程碑** M0

## 背景

OAR 的核心资产不是 adapter，是**一条有序、无损、带归因的记录流**。它的证据是 `docs/spec/record-stream.md` 与被否决的两种设计（control-object 模型、双通道模型）。Go 里没有任何项目做过这件事——agentexec 给的是扁平 event，multica 给的是 `Message` 联合，都不带 seq/cursor/归因。

## 目标

写一份 Go 侧规范，冻结以下内容：

1. `Record` 结构：`Seq / ReceivedAt / Kind / SessionID / AgentPath / SpanID / Native / Events`。
2. 三种 kind 的义务划分（frame / request / response），**response 只在观察到运行时不会自己说的结果时存在**。
3. 五条不变量：不合成、不裁剪、可达性从流里读、空输入先拒、凭证不入流。
4. cursor 语义：`{SessionID, AfterSeq}`，先补历史再续直播；流实例重建后 seq 重置。
5. `toApp` 请求的方向判定（`direction` 是唯一判据，不解析 body）。

## 验收标准

- [ ] `docs/spec/record-stream.md` 落地，含结构图与五条不变量
- [ ] 每个不变量配一个"它防止的事故"（引用 OAR 的 evidence 或本仓 ADR-0001）
- [ ] cursor 语义明确写出"实例身份"与"seq 重置"的处理
- [ ] 至少 3 个反例：合成 turn_end、settled-gate 丢事件、双通道失去 total order
- [ ] 评审通过（本 issue 关闭即可开工 #6）

## 非目标

- 持久化格式（voyage 见 #17）
- 跨进程游标恢复（属于宿主）

## 参考

- OAR `docs/spec/record-stream.md`、`docs/design/decisions.md`
- 本仓 `docs/architecture.md` §1、`docs/adr/0001-three-kind-record-stream.md`

## 依赖

无（起点）
