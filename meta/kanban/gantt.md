# sluice — Kanban Gantt

## OpenAI compatibility increment (current)

```mermaid
gantt
    title OpenAI compatibility (ADR-0012, ADR-0013)
    dateFormat X
    axisFormat Day %j
    section Wave 1 (no deps)
        CARD-016 OpenAI upstream adapter :p1, card016, 0, 2d
    section Wave 2
        CARD-017 OpenAI-compatible edge  :p1, card017, after card016, 3d
```

CARD-016 ships first (real OpenAI upstream adapter; the edge stays simplified — a valid
intermediate state). CARD-017 then flips the edge to the real OpenAI shape (drop-in for
OpenAI SDKs). See `meta/architecture/handoff.md` §6.

_(v1 build waves 1–6 are complete — see the Done section of board.md.)_
