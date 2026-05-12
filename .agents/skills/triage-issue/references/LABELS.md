# Label Catalogue

This is the reference loaded by the `triage-issue` and `triage-backlog`
skills. Keep it in sync with the actual labels on the GitHub repository.
The agentskills.io spec recommends keeping this kind of detail in a
`references/` file so it's loaded only when needed.

---

## Issue type vs labels

**Issue type** (Bug / Enhancement / Epic / Task) is set via GitHub's native
type picker. It applies to **issues only** — GitHub's native type system
does not cover pull requests.

**`kind/*` labels** fill that gap for PRs. Apply exactly one per PR; they
map 1:1 to conventional commit prefixes:

| Label           | Commit prefix | When to use                              |
|-----------------|---------------|------------------------------------------|
| `kind/feat`     | `feat:`       | Adds new functionality                   |
| `kind/fix`      | `fix:`        | Fixes a bug                              |
| `kind/refactor` | `refactor:`   | Refactor without behaviour change        |
| `kind/chore`    | `chore:`      | Maintenance, deps, CI, tooling           |
| `kind/docs`     | `docs:`       | Documentation only                       |
| `kind/security` | `security:`   | Security fix or hardening                |
| `kind/test`     | `test:`       | Tests only                               |

---

## Triage checklists

For every new **issue**:

```
[ ] Issue type      Bug / Enhancement / Epic / Task   (GitHub type picker)
[ ] priority:*      critical / high / medium / low
[ ] area/*          one or more component labels
[ ] status/*        only if in a special state
[ ] effort:*        for enhancements with clear scope
[ ] good-first-issue / help-wanted   if applicable
```

For every new **pull request**:

```
[ ] kind/*          exactly one (feat/fix/refactor/chore/docs/security/test)
[ ] area/*          one or more component labels
[ ] priority:*      only if urgency needs to be surfaced
[ ] effort:*        optional
```

---

## Priority

Required on every issue after triage. Exactly one.

| Label               | When                                                  |
|---------------------|-------------------------------------------------------|
| `priority:critical` | Do today — system down, security, data loss          |
| `priority:high`     | Do this sprint — major functionality broken           |
| `priority:medium`   | Standard sprint work                                   |
| `priority:low`      | Defer when capacity allows                             |

---

## Area

Optional. One or more.

**Core**

| Label         | Scope                                                          |
|---------------|----------------------------------------------------------------|
| `area/agent`  | Agent lifecycle, handoffs, delegation, orchestration           |
| `area/tools`  | Built-in tools (filesystem, shell, think, todo, memory, …)     |

**Interfaces**

| Label       | Scope                                              |
|-------------|----------------------------------------------------|
| `area/tui`  | Terminal UI, keybindings, rendering, interactions  |
| `area/cli`  | CLI commands, flags, output formatting             |

**Integration & extensibility**

| Label          | Scope                                                  |
|----------------|--------------------------------------------------------|
| `area/mcp`     | MCP protocol, MCP tool servers, integration            |
| `area/api`     | API exposure, server mode, programmatic access         |
| `area/rag`     | Retrieval-augmented generation features                |
| `area/skills`  | Skills system and custom slash commands                |

**Providers**

| Label                                  | Scope                                            |
|----------------------------------------|--------------------------------------------------|
| `area/providers`                       | General LLM provider / multi-provider concerns   |
| `area/providers/anthropic`             | Anthropic (Claude)                                |
| `area/providers/openai`                | OpenAI (GPT, o-series)                            |
| `area/providers/docker-model-runner`   | Docker Model Runner local inference               |
| `area/providers/bedrock`               | AWS Bedrock                                       |
| `area/providers/gemini`                | Google Gemini                                     |
| `area/providers/other`                 | Mistral, xAI, Codex, etc.                         |

**Infrastructure**

| Label                | Scope                                                |
|----------------------|------------------------------------------------------|
| `area/config`        | Configuration parsing, YAML, env vars                |
| `area/sessions`      | Session persistence, resume, export, lifecycle       |
| `area/distribution`  | Agent registry, packaging, distribution, sharing     |
| `area/security`      | Authentication, authorization, secrets               |
| `area/testing`       | Test infrastructure, CI/CD, runners, evaluation      |

---

## Status

| Label                     | When                                              |
|---------------------------|---------------------------------------------------|
| `status/needs-triage`     | Not yet reviewed                                  |
| `status/needs-info`       | Waiting on reporter                               |
| `status/needs-design`     | Architecture discussion before implementation     |
| `status/in-progress`      | Being actively worked on                          |
| `status/blocked`          | External dependency                               |
| `status/ready-for-review` | Ready for next phase                              |
| `status/duplicate`        | Duplicate of another issue/PR                     |
| `status/wontfix`          | Will not be addressed                             |

---

## Effort

Optional. Helps contributors self-select.

| Label            | Scope                                  |
|------------------|----------------------------------------|
| `effort:small`   | 1–4 hours, straightforward             |
| `effort:medium`  | half-day to 2 days, moderate           |
| `effort:large`   | 2+ days, significant complexity        |

## Contribution

| Label              | When                                                     |
|--------------------|----------------------------------------------------------|
| `good-first-issue` | Clear scope, well-documented, suitable for newcomers     |
| `help-wanted`      | Extra attention sought from the community                |

## Language / file type (PR-only, often automated)

| Label          | Scope                                  |
|----------------|----------------------------------------|
| `go`           | PR updates Go code                     |
| `python`       | PR updates Python code                 |
| `dependencies` | PR updates a dependency manifest       |
| `automated`    | Issue/PR created by an automated tool  |

---

## Useful `gh` queries

```sh
gh issue list --search "type:Bug label:priority:high"
gh issue list --search "label:good-first-issue label:effort:small"
gh issue list --search "label:status/needs-triage"
gh pr list    --search "label:kind/security"
gh pr list    --search "label:kind/feat label:area/providers"
gh issue list --search "label:area/config label:priority:high"
```
