# Benchmarks + redaction fuzz

**优先级** P2 · **区域** test / security · **类型** quality · **里程碑** M10

## 背景

核心路径已有 kernel observer benchmark，但 folds、streamjson、PromptAndWait 没有基准；redaction 只有表驱动测试，没有 fuzz。两者都是长期回归防线。

## 目标

1. 增加 benchmark：
   - `internal/fold`：10k records 的全量 fold 与增量 Feed；
   - `internal/streamjson`：大帧/多帧 fold；
   - `internal/kernel` 现有 ObserveBuffered 基准保留；
   - `PromptAndWait` 可在 fake session 上做延迟基准（可选）。
   在 `docs/` 或注释里写如何运行；不做脆弱的 CI 时间阈值。
2. 增加 fuzz：
   - `FuzzRedactor_Text`：任意字符串不 panic；已知 secret 必须被替换；安全名称（PATH/HOME/SSH_AUTH_SOCK）不被误杀。
   - `FuzzRedactor_Native`/`Bytes`：任意 JSON/非 JSON 输入不 panic；合法 JSON 输出仍合法；无凭证时不改变输入。
   - 种子语料覆盖 URL 密码、Authorization、env/headers、短值/长值。
3. 只加测试/文档；不改生产逻辑；不加第三方依赖。
4. `go test -race ./...` 全绿；fuzz 默认短跑，CI 不跑长 fuzz。

## 验收标准

- [ ] 至少 3 个 benchmark 可运行（`go test -bench`）
- [ ] 2 个 fuzz target，种子语料齐全，默认 `go test` 通过
- [ ] fuzz 断言覆盖“不 panic / 必脱敏 / 不误杀”三类
- [ ] gofmt/vet/test -race/Windows build 全绿

## 非目标

- 性能优化本身
- 网络/模糊 vendor CLI

## 参考

- `internal/fold/*`、`internal/streamjson/*`、`internal/redact/*`
- `internal/kernel/observe_buffered_bench_test.go`

## 依赖

#7、#4（已合入）
