# `meta/` — the architecture and the build log

This folder is part of why sluice is a *reference* repo and not just clean code: it is the
**architecture-as-code** behind the service, plus the **honest log of how it was built**.
You can read the entire "why" and "how" without reverse-engineering it from the source.

If you only want the runtime narrative, the human-readable version lives in
[`docs/how-it-works/`](../docs/how-it-works/) (mechanism) and [`docs/role/`](../docs/role/)
(user-facing). This folder is the machine-checked model those docs are generated from.

## What's here

| Path | What it is |
|------|------------|
| [`architecture/decisions/adr/`](architecture/decisions/adr/) | **ADRs** — every significant decision, its context, options, and consequences. |
| [`architecture/c4/`](architecture/c4/) | **C4 diagrams** (PlantUML): system context, containers, and per-context components. |
| [`architecture/domain/`](architecture/domain/) | The **domain model**: bounded contexts, capabilities, aggregates, events/commands, actors, the context map. |
| [`architecture/requirements.yml`](architecture/requirements.yml) | Functional / non-functional requirements and constraints (FR/NFR/CON). |
| [`architecture/trace.yml`](architecture/trace.yml) | **Traceability**: requirements ↔ capabilities ↔ components ↔ source files ↔ tests. |
| [`kanban/`](kanban/) | The **build log**: the board, per-task cards (with their architecture context and review outcome), the roadmap, and the per-wave retrospective. |

The model is kept consistent with the code: a drift gate validates the architecture model,
and the trace links requirements through to the files and tests that satisfy them.

## How this repo was built (provenance — we own it)

sluice was produced with an **architecture-first, AI-assisted methodology** (the
[`forge`](https://github.com/Adverax/forge) skill pipeline): a raw spec → domain extraction
→ ADRs → C4 → a kanban decomposition → implementation with a **severity-gated review loop**
→ retrospectives. The artifacts in this folder are the real, unedited output of that
process. We keep them public deliberately — the decision trail *is* the reference value.

A couple of honest reading notes for [`kanban/`](kanban/):

- **`Actual:` on a card is AI wall-clock, not human-days.** The `Estimate` field is a
  conventional human-day sizing; the `Actual` (e.g. `0.1d`) is how long the AI agent took.
  The retrospectives say so explicitly — accuracy-based calibration is meaningless at that
  granularity, so quality is tracked via **review cycles** and **final score** instead.
- **The review loop caught real defects.** The retro (`kanban/retro.md`) records that the
  severity gate blocked ~40% of cards on genuine issues (a rate-limit DoS, an auth bypass,
  response-body corruption, a billing data-loss bug, metric-cardinality blowups) — all fixed
  before merge. That's the loop doing its job, not a blemish.
- The later cards (CARD-013/014/015) and `doc/requirements-audit.md` are a **post-v1
  conformance pass**: an audit of the build against the original spec, and the hardening that
  closed every gap it found.

In short: this folder is meant to be read. It's the part that turns "a working gateway" into
"a worked example."
