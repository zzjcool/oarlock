# Roadmap

Estimates assume one experienced Go engineer who has read OAR's specs. Two
engineers parallelise across milestones, not inside one.

| Milestone | Theme | Exit criteria | Estimate |
|---|---|---|---|
| **M0** | Specifications | Six decisions are written and reviewed; the capability/error model compiles as a design | 1 week |
| **M1** | Pi end-to-end | `Prompt` → records → `Status`/`Usage` on a real Pi; abort, dispose, resume all evidenced | 3–4 weeks |
| **M2** | Kernel hardened + Claude | golden traces, scripted runtime, process/platform layer, second adapter on the same contract | 3 weeks |
| **M3** | Reach | Codex app-server, ACP family (5), cursor CLI with declared degradations | 4–5 weeks |
| **M4** | Operations | install / login / logout / usage / upgrade / inventories, each typed and honest | 4–6 weeks |

Total to end of M3 (the "usable" line): **11–13 weeks, one engineer**.

## M0 — Specifications (week 1)

- [#1](issues/0001-record-stream-spec.md) record stream
- [#2](issues/0002-capability-model.md) capability model
- [#3](issues/0003-error-taxonomy.md) error taxonomy
- [#4](issues/0004-redaction-spec.md) redaction

**Gate:** no code until #1–#3 are agreed. These are the decisions that cannot be
changed later without breaking every consumer's stored data.

## M1 — Pi, end to end (weeks 2–5)

Order matters; each blocks the next.

```
#5 spike  →  #9 transport  →  #10 controls  →  #11 projection
                    ↓                ↓
                 #6 kernel       #7 folds
                    ↓
                 #8 observers
```

Then #12 resume, #13 readback, #14 inventories, #15 version gate, #16 platform.

**Gate:** a real Pi session must produce a stream a second process can replay
from a cursor and reach the same status and usage.

## M2 — Kernel hardened + Claude (weeks 6–8)

#17 voyage + fixtures, #18 scripted runtime, #19 Claude.

**Gate:** Claude and Pi pass the *same* behaviour suite; the Claude adapter adds
no new kernel code.

## M3 — Reach (weeks 9–13)

#20 Codex, #21 ACP family, #22 Cursor.

**Gate:** ten runtimes, each with a golden trace and a declared capability row.

## M4 — Operations (weeks 14–19, optional)

#23 install/login/usage/upgrade.

Deliberately last: these are "dirty work" — a mistake stays local, and every
vendor changes them often. Users can sign in with the vendor CLI until then.

## Standing risks

| Risk | Mitigation |
|---|---|
| Upstream OAR moves 8 releases/week | Sync behaviour, not code: regenerate golden traces, run the differential suite |
| Vendor CLI ships a breaking stream change | unknown-event tally + fixture diff turns it into a red test, not a silent gap |
| Codex app-server schema churn | pin a schema revision; refusal outside the window |
| Pi RPC is JS-only | single-file host documented; the Go module stays dependency-free |
| Scope creep into orchestration | non-goals in README; queues/DAGs/worktrees belong to the host |
