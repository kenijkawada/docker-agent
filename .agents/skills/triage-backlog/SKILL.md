---
name: triage-backlog
description: |
  Batch-triage a set of open GitHub issues or pull requests in one go. Reads
  each item, applies the single-item rules from triage-issue, and produces a
  copy-pastable summary table with what was classified, what was skipped,
  and what needs human attention. Use when the user asks for a backlog
  cleanup, weekly triage, "go through everything labelled
  status/needs-triage", or types /triage-backlog.
context: fork
model: sonnet
---

# Batch-Triage the Backlog

This skill is for processing **many issues or PRs** in one pass. It runs as
a fork sub-agent so the chatty MCP traffic doesn't flood the parent
conversation. The single-item rules in `triage-issue` still apply per item.

## 1. Scope the batch

Ask the user (or infer from the request) which set you're triaging.
Common targets:

- `status/needs-triage` open issues.
- All issues opened in the last N days.
- All open PRs missing a `kind/*` label.
- A specific milestone or area.

Build the `gh` query and confirm it before fetching:

```sh
gh issue list --search "label:status/needs-triage state:open" --limit 100
gh pr list    --search "no:label kind/" --limit 50
```

## 2. Plan first

Print the plan once, before any mutation:

```
Backlog triage plan
- Source: open issues with status/needs-triage (42 items)
- For each: apply type / priority / area / status (per triage-issue rules)
- Reference: .agents/skills/triage-issue/references/LABELS.md
- Output: summary table, no auto-routing
- STOP after dry-run; await approval before applying labels.
```

Wait for explicit user approval before mutating GitHub.

## 3. Process each item

For every item in the batch:

1. Fetch the issue/PR (`get_issue` or `get_pull_request` MCP tool).
2. Run the `triage-issue` decision tree (type, priority, area, status,
   route).
3. Record the proposed labels in the running table — **do not apply yet**.
4. If the item is missing critical info, mark it `needs-info` in the
   table; do not invent a priority.

Keep one row per item. Don't write per-item analysis paragraphs — the
summary table is the output.

## 4. Summary table

Output as a fenced markdown block, copy-pastable into a PR or comment:

~~~
```markdown
| #    | Title                          | Type        | Priority | Areas                  | Route          | Notes              |
|------|--------------------------------|-------------|----------|------------------------|----------------|--------------------|
| 412  | TUI freezes on /clear          | Bug         | high     | area/tui               | troubleshooter |                    |
| 415  | Add Bedrock streaming          | Enhancement | medium   | area/providers/bedrock | architect      |                    |
| 421  | (no description)               | —           | —        | —                      | —              | needs-info         |
| 423  | Bump golang.org/x/crypto       | —           | —        | dependencies           | (auto)         | dependabot, skip   |
```
~~~

The table is the *single most important deliverable*. Make it accurate.

## 5. Apply (after approval)

Once the user approves the table, walk it row by row and apply changes
via the GitHub MCP. Skip rows that need human judgement (`needs-info`,
"unsure" rows). Report progress every 10 items.

After the run, post a final report:

- `<n>` items labelled
- `<m>` items left as `status/needs-info` with comments
- `<k>` items deferred for human triage (list the numbers)

## 6. Failure handling

- If the GitHub MCP rate-limits, pause and report — don't retry blindly.
- If a label doesn't exist on the repo, surface it; do not create labels
  silently. (Label creation is a separate, deliberate action.)
- If you're more than 50% unsure on an item, defer it rather than guess.

## 7. When to use a different skill

- One issue only → `triage-issue` (inline, faster).
- Reviewing a single PR's diff → `review-pr`.
- Daily "what's on my plate" → handled by `issue_manager` directly, not
  this skill.
