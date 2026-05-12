---
name: research-topic
description: |
  Search the web for technical documentation, articles, code examples, RFCs,
  and best practices on a focused topic, then return a concise summary with
  links and the most relevant excerpts. Replaces the standalone librarian
  agent. Use when the user (or another skill) needs prior art, library docs,
  protocol details, error explanations, or pattern references, or types
  /research-topic.
context: fork
model: haiku
---

# Research a Topic

Runs as a fork sub-agent on haiku. Research is bursty and tool-heavy
(many web fetches, read-and-discard); haiku is plenty for the
read-and-summarise task and keeps cost low. Only the curated summary
folds back to the parent agent.

This skill **replaces** the previous `librarian` agent: any agent that
needs research now invokes this skill instead of handing off.

## Required tools

Callers must have at least one of:

- `mcp:web-search` (preferred — for the search step)
- `fetch` (for retrieving specific URLs the user already knows)

If neither is available, fail fast with a clear message — don't
hallucinate sources.

## 1. Clarify the question

If the request is vague, ask one focused question before searching.
Examples of usefully focused questions:

- What's the language / framework / version?
- Are you looking for the official docs or community examples?
- What have you already tried (so I don't repeat it)?

If the request is already specific, skip and start searching.

## 2. Plan first

Output a one-line plan:

```
Research plan: 3 web searches, fetch up to 5 sources, return curated summary
- Topic: "Anthropic prompt-caching limits"
- Filters: prefer docs.anthropic.com and recent posts (<6 months)
- STOP — search runs, present summary, no side effects
```

This skill performs **only read-only network calls**; no approval needed
to run searches and fetches once the plan is shown.

## 3. Search

Run the searches. Cast a small net first; widen only if needed.

- Prefer **official documentation** over blog posts.
- Prefer **recent** content (note dates; flag if a source is >2 years
  old in a fast-moving area).
- Cross-reference at least two independent sources for any factual
  claim.
- For error messages: search the literal string in quotes;
  cross-reference with GitHub issues.
- For library APIs: prefer the project's own docs/repo over
  third-party tutorials.

## 4. Filter and rank

For each candidate source, evaluate:

- **Authority** — official > recognised expert > blog > random forum.
- **Recency** — note the publication / last-updated date.
- **Specificity** — does it answer *this* question, or just touch on
  the topic?
- **Reproducibility** — does it include working examples / code?

Drop everything that doesn't earn a place in the top ~5 sources.

## 5. Summarise

The fold-back to the parent should be **short and actionable**.
Format:

```markdown
## Topic: <one-line restatement of the question>

**Key findings**
- <bullet 1, with the source URL inline>
- <bullet 2, …>
- <bullet 3, …>

**Recommended approach** (if the question implied one)
- <one paragraph or numbered list>

**Caveats**
- Version notes, deprecations, gotchas.

**Sources**
1. <Title> — <url> (date, authority)
2. <Title> — <url> (date)
3. …
```

Keep total length under ~40 lines. The user / parent agent reads this
inline; if they want detail they follow the links.

## 6. Confidence

Always include a confidence indicator:

- **High** — multiple authoritative, recent sources agree.
- **Medium** — one authoritative source, or several non-authoritative
  sources agreeing.
- **Low** — speculation, conflicting sources, very recent / niche
  topic. Recommend the user verify before acting.

Never bluff confidence. "I couldn't find a definitive answer" is a
valid result.

## 7. When this skill is the wrong tool

- For information already in the codebase → the parent agent should
  just read the code (`grep`, `read_file`).
- For internal / private knowledge bases → not in scope; this skill
  searches the public web only.
- For long-running competitive analysis → escalate; this skill is for
  focused, single-question lookups.
