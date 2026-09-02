# [wraith CI] skill-gate: general left-anchored review-verdict grep + selftest (port of yoru 3cb49b51)

## Team : cto-tsukumo (tsukumo)
## Branch : cto-tsukumo/skill-gate-general-verdict-grep (from main)
## Relay task : 41ff3271-85ef-4dad-bec4-3594bbb55855
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. grep pattern byte-identical to overnight-saas origin/main skill-gate.yml: (^|[^a-z0-9-])review-[a-z0-9-]+( verdict)?:|anti-ai-slop
- [ ] 2. verdict-grep-selftest job present with the 10 pinned samples (3 match / 7 no-match)
- [ ] 3. YAML parses; only .github/workflows/skill-gate.yml changes

## 2. Root cause & decisions

ROOT_CAUSE: skill-gate.yml greps an enumerated review-(trovex|wraith|dokan|yoru|tsukumo|donna) lane list, so every domain-scoped review skill (review-backend-api, review-backend-dashboard, ...) fails the 'review-skill verdict present' check even with a valid verdict block. niwa provisioning never writes target-repo workflows (niwa-cto 08a74f46 moot), so the fix is repo-side.

DECISION: port helios-code/overnight-saas origin/main skill-gate.yml verbatim (yoru task 3cb49b51, merged 1140bef8): left-anchored general pattern (^|[^a-z0-9-])review-[a-z0-9-]+( verdict)?:|anti-ai-slop plus a verdict-grep-selftest job pinning 3 match / 7 no-match samples. Byte-identical across repos so the next regex change is one diff.

REJECTED: (a) unanchored review-[a-z0-9-]+ — false-positives on pr-review-self / code-review-checklist / #pullrequestreview-N (proven in 3cb49b51 r1). (b) colon-only anchor — still false-positives on 'pr-review-self: done'. (c) adding lanes to the enumeration — reintroduces the class on the next scoped skill.

## review-wraith verdict: SHIP
CI-only change, one file. Regex selftest run locally: 3/3 match, 7/7 no-match. YAML parses. No product code, no secrets, no schema.

## 3. Files changed

```
.github/workflows/skill-gate.yml | 49 +++++++++++++++++++++++++++++++++++++++-
 1 file changed, 48 insertions(+), 1 deletion(-)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `41ff3271-85ef-4dad-bec4-3594bbb55855`._
