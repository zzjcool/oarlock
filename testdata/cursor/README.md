# Cursor Agent stream-json traces

Recorded on 2026-10-28 from `cursor-agent` version `2026.09.18-9a7762b`
(logged in as a normal user account), with the prompt on **stdin**:

```sh
printf '<prompt>' | cursor-agent -p --output-format stream-json \
  --stream-partial-output --force --trust --sandbox disabled --model auto
```

| file | what it contains |
|---|---|
| `2026.09.18-auto-normal.stream-json.jsonl` | init, user echo, assistant text deltas, top-level `thinking` deltas, final `result` with usage |
| `2026.09.18-auto-shell-tool.stream-json.jsonl` | the same, plus top-level `tool_call` started/completed for a shell call (`echo hi`), including its exit code and stdout |

`cwd` was normalized from the probe directory to `/repo`. Session/request ids
are vendor-generated UUIDs, not credentials; no API key or Authorization
header appears in the traces. They are the evidence for issue #22.
