# 脱敏契约：名称规则 / URL 密码 / 结构化字段 / 错误文本

**优先级** P1 · **区域** security · **类型** spec · **里程碑** M0

## 背景

OAR 在**保留与分发之前**做脱敏，现场观察与回放看到同一份。它的规则比"过滤 API_KEY"细得多：名称后缀白名单、值长度/路径排除、连接串内嵌密码、MCP `env`/`headers`、Authorization 的 scheme 之后部分、错误 `message/stack/cause`。漏一处就是安全事故。

## 目标

1. 名称规则：最后一段下划线分段属于 `KEY/APIKEY/TOKEN/SECRET/PASSWORD/PASSWD/PASSPHRASE/CREDENTIAL/CREDENTIALS/COOKIE/PWD`（忽略大小写），额外 `PGPASSWORD`。
2. 值规则：长度 ≥8；排除空值、`/`、`~/`、盘符路径；替换为 `[redacted]`，**按长度从长到短**。
3. 结构规则：`env` / `headers` / `Authorization: Bearer X` 的 scheme 之后部分。
4. URL 规则：带非空密码的连接串整体保护；URL 内编码/解码密码也保护。
5. 错误文本：`message` / `stack` / 结构化 `cause` 同规则。
6. 反向保证：不做过度脱敏——`SSH_AUTH_SOCK`、`GIT_AUTHOR_EMAIL`、`AWS_ACCESS_KEY_ID`、`HOME`、`PATH` 必须保持可读。

## 验收标准

- [ ] 表驱动测试：40+ 组（保护 / 不保护）样例
- [ ] 一条"最短优先"的反例：`sk-abc` 与 `sk-abcdef123` 同时存在
- [ ] 声明"不做的事"：不扫描宿主环境、不清理历史日志、不处理被 delta 拆分的片段
- [ ] 性能约束：无凭证命中时不复制、不遍历

## 非目标

- 传输层 TLS、沙箱
- 供应商自己的日志

## 参考

- OAR `docs/spec/record-stream.md#the-rules`（Credential 段落）
- 本仓 ADR-0006

## 依赖

#1
