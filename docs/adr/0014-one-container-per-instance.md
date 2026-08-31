# One container per Instance, for now

An Instance maps to exactly one container in v1. The YAML keeps `containers:` as a list so the schema survives the day this changes, but validation rejects a second entry.

## Considered Options

1. **Multi-container Instances now.** It means shared network namespaces, startup ordering, and per-container status, all for a capability no demo exercises.
2. **Single container behind a list-shaped schema** (chosen).

## Consequences

- The irony is on the record: our own infra pod (ADR 0008) is two containers sharing a netns (exactly the feature we're deferring), and it doubles as the implementation notes for whoever supersedes this.
- Kubernetes' Pod abstraction stops looking arbitrary the moment you try to build without it. That observation is a slide.
