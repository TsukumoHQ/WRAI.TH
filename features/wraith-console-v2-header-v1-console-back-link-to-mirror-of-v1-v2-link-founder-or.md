# [wraith/console] v2 header: 'v1 console' back-link to / (mirror of v1 #v2-link) — founder order 11:30Z

## Team : wraith-engine-2 (tsukumo)
## Branch : wraith-engine-2/v2-v1-link (from main)
## Relay task : bd7b7922-9c34-49ed-a2d2-bd7ad56a0be9
## Status : 🔵 SUBMITTED

## 1. Product Brief

### Acceptance Criteria
- [ ] 1. AC1 named test: static/v2/index.html contains an <a> with id="v1-link", href="/", class containing back-chip, a non-empty aria-label, and no tabindex attribute (keyboard-reachable); the element sits inside the hdr-right div
- [ ] 2. AC2 named test: the string v1-link appears in NO v1 asset (static/index.html, static/style.css, static/js/main.js) and v1's existing id="v2-link" anchor (href="/v2/") is still present unchanged
- [ ] 3. AC3 named smoke test: the v2 index still contains the live status span id="liveStatus" inside hdr-right after the link (layout preserved); scope: diff touches exactly internal/web/static/v2/index.html + internal/relay/console_v2_v1_link_test.go; go test -tags fts5 ./internal/relay/... green

## 2. Root cause & decisions

# Decision — bd7b7922 [wraith/console] v2 header 'v1 console' back-link

ROOT_CAUSE: v2 console had no way back to the v1 console; founder order 11:30Z
(cto-tsukumo msg 9118bd5e T1: "il faut un bouton pour revenir a la v1 depuis la v2").

## Decision
Add a back-link `<a id="v1-link" class="back-chip" href="/">v1 console</a>` inside the
v2 header's `<div class="hdr-right">`, before the live-status span. Mirror of the v1
`#v2-link` anchor (internal/web/static/index.html:70). Reuse the existing `.back-chip`
class already in v2.css (used by the project back link) so v2.css is NOT touched. Plain
`<a href>` = natively keyboard-focusable, so no tabindex and no JS.

## Files (2)
- internal/web/static/v2/index.html — +1 line, the anchor
- internal/relay/console_v2_v1_link_test.go — new, 3 named tests (readCfgAsset pattern)

## ACs → tests
- AC1 TestV2V1LinkPresent — anchor id=v1-link, href=/, class has back-chip, non-empty
  aria-label, no tabindex, inside hdr-right.
- AC2 TestV2V1LinkNoV1Leak — id v1-link in NO v1 asset; v1 id=v2-link (href=/v2/) intact.
- AC3 TestV2V1LinkLayoutPreserved — liveStatus span still in hdr-right, after the link.

## Rejected alternatives
- New CSS class / edit v2.css: rejected — .back-chip already fits, keeps diff to 2 files.
- Button + JS navigation: rejected — plain anchor is natively focusable, zero JS.

## Verify
go test -tags fts5 ./internal/relay/... → 469 passed (3 new). build/vet/gofmt clean.

No LEGACY_OPPORTUNITY. v1 assets byte-identical.

## review-wraith verdict: SHIP
Scope: internal/web/static/v2/index.html (+1 line anchor), internal/relay/console_v2_v1_link_test.go (new, 3 tests)
Gate: build -tags fts5 OK / vet OK / gofmt OK / test -tags fts5 OK (469 passed, 3 new)

BLOCKERS (must fix before merge):
- none

NITS (non-blocking):
- none

Notes: static asset + test only. No DB/schema/writer/messaging/dispatch/MCP/updater
surface touched — thesis (SSOT, single-writer, backward-compat) not in scope. v2.css
untouched (reused .back-chip). v1 assets byte-identical (AC2 asserts no v1-link leak +
v2-link intact). Plain anchor, no tabindex/JS = natively keyboard-reachable.

## 3. Files changed

```
internal/relay/console_v2_v1_link_test.go | 103 ++++++++++++++++++++++++++++++
 internal/web/static/v2/index.html         |   1 +
 2 files changed, 104 insertions(+)
```

## 4. QA Log

_(no review round yet)_

## 5. Timeline


---
_Auto-assembled by the niwa scribe from the Q&A gate. Task `bd7b7922-9c34-49ed-a2d2-bd7ad56a0be9`._
