---
name: triage-issue
description: |
  Triage a single GitHub issue or pull request: validate it has enough
  information, set the issue type via GitHub's type picker (Bug / Enhancement
  / Epic / Task), apply exactly one priority label, attach area labels, set
  status if special, and decide who to route it to. Use when the user asks
  to triage an issue, classify a bug report, label a PR, or types
  /triage-issue.
---

# Triage One Issue or PR

Single-item triage — fast, deterministic, ≤ a couple of minutes per issue.
For batch triage of many open issues at once, use `triage-backlog` instead.

The full label catalogue lives in `references/LABELS.md`. Read it the first
time you triage in a session, or whenever you're unsure which `area/*` label
applies.

## 1. Plan first

Before mutating anything on GitHub, post a short plan and wait for approval:

```
Triage plan for #123
- Type:     Bug   (reason: failing behaviour described)
- Priority: high  (reason: blocks all users on macOS)
- Areas:    area/tui, area/sessions
- Status:   (none — actionable as is)
- Route to: troubleshooter
```

Only proceed once the user says go. The plan-first rule is a hard rule for
this team.

## 2. Validate the report

For an **issue** to be actionable:

- Clear, descriptive title.
- Description with context: what was attempted, what happened, what was
  expected.
- For bugs: reproduction steps, environment info, logs or screenshots when
  relevant.

If anything critical is missing, **don't classify**. Apply
`status/needs-info`, leave a polite comment listing the gaps, and stop.

For a **pull request**: skip type/priority unless urgency matters; jump
straight to `kind/*` + `area/*` (see step 5).

## 3. Set the issue type

Use GitHub's native type picker (this is **not** a label):

| Type | When |
|---|---|
| `Bug` | Existing behaviour is broken |
| `Enhancement` | New feature or improvement |
| `Epic` | Multi-issue umbrella |
| `Task` | Internal work (chore, refactor, docs) |

GitHub MCP tool: `update_issue` with the `type` field, or use the GraphQL
mutation `updateIssueType` if the REST tool doesn't support it yet. When in
doubt, leave the type unset and flag it for the user.

## 4. Apply exactly one `priority:*` label

| Label | Meaning |
|---|---|
| `priority:critical` | System down, security, data loss — do today |
| `priority:high` | Major functionality broken — this sprint |
| `priority:medium` | Standard sprint work |
| `priority:low` | Nice-to-have, defer |

Decision shortcuts:
- Crash / data-loss / CVE → `critical`.
- Blocks an entire workflow for many users → `high`.
- Single-user nuisance / edge case → `medium`.
- Cosmetic / cleanup → `low`.

## 5. Apply one or more `area/*` labels

Pick the smallest set that accurately covers the affected components. See
`references/LABELS.md` for the full taxonomy and one-line guidance per
label.

For PRs, add **exactly one `kind/*` label** matching the conventional
commit prefix (`kind/feat`, `kind/fix`, `kind/refactor`, `kind/chore`,
`kind/docs`, `kind/security`, `kind/test`).

## 6. Apply a `status/*` only when special

Most issues don't need a status label. Apply one only if it tells the
reader something they wouldn't infer from open/closed:

- `status/needs-info` — waiting on the reporter.
- `status/needs-design` — architecture call required first.
- `status/blocked` — external dependency.
- `status/duplicate`, `status/wontfix` — when closing as such.

Skip `status/needs-triage` once you've triaged.

## 7. Route

Suggest the next agent and explain why in one line:

| Issue type | Default route |
|---|---|
| Bug | `troubleshooter` |
| Enhancement / Epic | `architect` |
| Task (docs) | `doc_writer` |
| Task (chore / tooling) | depends — usually back to `root` |

Do **not** hand off automatically; surface the suggestion to the user.

## 8. Apply

Once the user approves the plan, apply changes via the GitHub MCP:

- `add_labels_to_issue` — append `priority:*`, `area/*`, optional `status/*`,
  `effort:*`, `good-first-issue`, `help-wanted`.
- Set issue type via `update_issue` (or comment with `/type Bug` if the bot
  is configured).
- If the issue lacks info, post a comment listing what's missing and add
  `status/needs-info`.

Report back: what was applied, what was skipped, any open question.
