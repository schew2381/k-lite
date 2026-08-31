# Decision Log

This directory tracks the design decisions made while building **k-lite** (a lite version of Kubernetes), including the tradeoffs and reasoning behind each one.

## Purpose

1. **During development** — record each significant decision as it's made, while the context is fresh.
2. **Afterward** — generate a presentation walking through the decisions and why they were made. Each record is structured so it maps roughly 1:1 onto a slide.

## How it works

- Each decision gets its own file: `NNNN-short-title.md` (e.g. `0001-scheduler-design.md`), numbered in the order decisions were made.
- Copy `TEMPLATE.md` to start a new record.
- The index below is updated with every new decision.
- A decision is never edited to pretend it didn't happen. If a decision is reversed, mark it **Superseded** and link the new record — reversals are often the most interesting slides.

## Record structure (why these fields)

| Field | Slide purpose |
|---|---|
| Context | "What problem were we facing?" |
| Options considered | "What could we have done?" |
| Decision | "What we chose" (the slide headline) |
| Tradeoffs | "What we gave up / accepted" |
| Rationale | "Why" — the speaker notes |
| Outcome | Filled in later: did it hold up? |

## Index

<!-- Add one line per decision, newest last. Keep this table in sync. -->

| # | Decision | Status | Date |
|---|----------|--------|------|
| — | *(no decisions recorded yet)* | | |
