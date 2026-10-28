#!/usr/bin/env bash
# Create the GitHub Projects (v2) board.
# Requires a token with the `project` scope:
#   gh auth refresh -s project,read:project
# Then:  ./scripts/setup-board.sh
set -euo pipefail

REPO="zzjcool/oarlock"
OWNER="zzjcool"
TITLE="oarlock"

proj=$(gh project create --owner "$OWNER" --title "$TITLE" --format json)
num=$(printf '%s' "$proj" | python3 -c 'import json,sys;print(json.load(sys.stdin)["number"])')
pid=$(printf '%s' "$proj" | python3 -c 'import json,sys;print(json.load(sys.stdin)["id"])')
echo "project #$num"

gh project link "$num" --owner "$OWNER" --repo "$REPO" || true
gh project field-create "$num" --owner "$OWNER" --name Priority \
  --data-type SINGLE_SELECT --single-select-options "P0,P1,P2,P3" >/dev/null || true

python3 - "$num" "$pid" "$OWNER" "$REPO" <<'PY'
import json, subprocess, sys
num, pid, owner, repo = sys.argv[1:5]

def gh(*a):
    return subprocess.run(["gh", *a], capture_output=True, text=True, check=True).stdout

fields = json.loads(gh("project", "field-list", num, "--owner", owner, "--format", "json"))["fields"]
pf = next(f for f in fields if f["name"] == "Priority")
opts = {o["name"]: o["id"] for o in pf["options"]}

issues = json.loads(gh("issue", "list", "-R", repo, "--limit", "200",
                       "--json", "number,url,labels"))
for i in sorted(issues, key=lambda x: x["number"]):
    p = next((l["name"].split("/")[1].upper()
              for l in i["labels"] if l["name"].startswith("priority/")), "P3")
    item = json.loads(gh("project", "item-add", num, "--owner", owner,
                         "--url", i["url"], "--format", "json"))["id"]
    gh("project", "item-edit", "--project-id", pid, "--id", item,
       "--field-id", pf["id"], "--single-select-option-id", opts[p])
    print(f"#{i['number']} -> {p}")

print(f"\nboard: https://github.com/users/{owner}/projects/{num}")
PY
