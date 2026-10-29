# 公共 API 收口：observe.Readback、examples/quickstart、README

**优先级** P0 · **区域** core · **类型** docs/api · **里程碑** M5

## 背景

`observe` 已提供 Status/Usage/Context/Model/Effort 五组 fold，但宿主要分别 type-assert 五个接口；仓库没有可运行的 quickstart；README 的示例刚补上 `Require`/`PromptAndWait`，需要一次端到端校验。

## 目标

1. 增加 `observe.Readback` 组合接口（嵌入五个 reader），所有 adapter 的 `*Session` 满足。
2. 增加 `examples/quickstart`：`Require` → `Installation` → `Session` → `PromptAndWait` → `Status/Usage/Context` → `Dispose`；可编译，live 部分 opt-in。
3. 增加 `docs/quickstart.md`，并在 README 链接；明确 `import _ ".../adapters/pi"` 的注册方式。
4. 评估 `PromptAndWait` 的 20ms 轮询是否改为 `Events` 订阅；若改，保持现有测试与语义不变，并补 caller-cancel 测试。
5. 检查 README 不再引用不存在的 API/模型名。

## 验收标准

- [ ] `examples/quickstart` 可 `go build`；live 运行时可用（至少 Pi）
- [ ] `observe.Readback` 可被至少 3 个 adapter 的 `*Session` 满足（编译期断言）
- [ ] README/docs 与实际 API 一致，无虚构函数
- [ ] `go test -race ./...`、gofmt/vet/Windows build 绿
- [ ] 若改轮询，原有 `PromptAndWait` 测试全过

## 非目标

- 新 adapter 能力
- 修改 issue #1–#23 的冻结语义

## 参考

- `promptwait.go`、`observe/observe.go`、`registry.go`
- README、`docs/architecture.md`

## 依赖

无
