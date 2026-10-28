# voyage 录制回放 + golden fixture 工装

**优先级** P1 · **区域** test · **类型** feature · **里程碑** M2

## 背景

OAR 的 `oar-voyage/3` 是"一行一个 JSON 记录"的自有格式，写入用同步 fd，崩溃也不丢已写部分。它是把"厂商行为"变成"可执行资产"的关键：录一次，回放永远。

## 目标

1. 写入器：`header` + 每条 `record` + `end`；同步写；无 `end` 即视为截断。
2. 回放器：`Replay(path) → []Record`，并驱动 fold 得到与 live 相同的 `Status/Usage`。
3. 脱敏在写入前完成（#4）。
4. fixture 管理：`testdata/<runtime>/<version>-<scenario>.jsonl` + 一个 `go:generate` 更新的索引。
5. 差分测试：同一份 fixture 喂给"旧实现"和"新实现"，断言折叠结果一致。

## 验收标准

- [ ] 录制一个真实 Pi 会话，回放后 `Status/Usage` 与现场一致
- [ ] 截断文件（无 `end`）能被识别，不 panic
- [ ] 回放不依赖任何网络/进程
- [ ] fixture 体积上限与脱敏检查（CI 中 grep 已知凭证模式）
- [ ] 文档：如何录制、如何升级 fixture

## 非目标

- 数据库/索引（宿主）
- 跨机器分发

## 参考

- OAR `packages/oar/src/voyage.ts`
- ADR-0006

## 依赖

#4、#7
