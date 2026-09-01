# k-lite

k-lite is a lite version of Kubernetes, with declarative YAML, a scheduling control plane, service discovery, and network policies on plain Docker.

## Seeing it

- [`frontend/`](frontend/README.md) is the live cluster UI: `cd frontend && bun install && bun run dev`. It runs on an in-browser simulator until klited lands (ADR 0023): topology with traced calls, an etcd browser, tables, logs, and policies.
- [`docs/design.html`](docs/design.html) is the interactive design walkthrough: the architecture, the data path, and how the design evolved round by round.

## Design records

The decision trail is a deliverable here, not a byproduct:

- [`CONTEXT.md`](CONTEXT.md) holds the glossary and says why a Workload is not a Deployment and an Instance is not a Pod.
- [`docs/adr/`](docs/adr/) keeps one record per design decision, rejected options and tradeoffs included.
- [`docs/design-log.md`](docs/design-log.md) tells how the design sessions actually went, reversals and all.
- [`research/`](research/) collects the tool-by-tool research behind the big choices.
