# k-lite

k-lite is a lite version of Kubernetes, with declarative YAML, a scheduling control plane, service discovery, and network policies on plain Docker.

## Design records

The decision trail is a deliverable here, not a byproduct:

- [`CONTEXT.md`](CONTEXT.md) holds the glossary and says why a Workload is not a Deployment and an Instance is not a Pod.
- [`docs/adr/`](docs/adr/) keeps one record per design decision, rejected options and tradeoffs included.
- [`docs/design-log.md`](docs/design-log.md) tells how the design sessions actually went, reversals and all.
- [`research/`](research/) collects the tool-by-tool research behind the big choices.
