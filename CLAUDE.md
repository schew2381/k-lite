# CLAUDE.md

## Git workflow

**Always commit directly to `main`.** Do not create feature branches or pull requests for this repo.

## Decision tracking

Design decisions are recorded as ADRs in `docs/adr/` and the glossary in `CONTEXT.md`, following the `domain-modeling` skill's formats (typically produced during `/grill-with-docs` sessions). One deviation from the minimal ADR format: **always include the Considered Options and Consequences sections** in this repo's ADRs — the decision log will later be turned into a presentation about the decisions made and their tradeoffs, so rejected alternatives and rationale must be captured even when they'd normally be skipped.

**Record every design and architecture decision, not just the ones made in grill sessions.** Any decision of consequence made during implementation, review, or debugging gets its ADR in the same commit as the change, while the alternatives are still fresh. If code and an ADR disagree, the code is wrong until a superseding ADR says otherwise. When vocabulary shifts, update `CONTEXT.md` in the same commit. Each design session appends an entry to `docs/design-log.md` recording what was recommended, what was chosen, and what got reversed.

## Writing quality in code

All prose written into code files — docstrings, comments, README text, decision records — must be evaluated with the personal `/writing` skill. Invoke it via an **adversarial agent**: spawn a subagent whose job is to critique the writing against the /writing skill's rules and flag violations (banned words, AI-tell structures, rhythm problems, etc.), not to rubber-stamp it. Apply this to any and all writing produced in this repo.

## Go Best Practices (Go 1.26)

### Formatting & Naming

- Always use `gofmt` (or `goimports`). Formatting is not a debate in Go.
- Package names: short, concise, lowercase (e.g., `bytes`, not `byte_buffer`). The package name acts as a natural prefix for its contents (`bytes.Buffer`).
- Variable names: MixedCaps (camelCase), no underscores. Keep names in short scopes brief (`i`, `r`).
- No `Get` prefixes on getters. An `Owner` field has getter `Owner()` and setter `SetOwner(owner)`.
- One-method interfaces end in `-er` (`Reader`, `Writer`, `Stringer`).

### Control Structures

- `switch` cases don't fall through (no `break` needed). A `switch` without an expression replaces if-else chains.
- Use `defer` for resource cleanup right next to the allocation.
- (Go 1.22+) Loop variables are per-iteration scoped. No need to shadow (`v := v`) for goroutine captures.
- (Go 1.22+) Range-over-int: `for i := range 10` replaces `for i := 0; i < 10; i++`. Use it for simple counted loops; stick with traditional `for` when you need a non-zero start or non-1 step.
- (Go 1.23+) Use standard iterators (`iter.Seq`) and range over custom functions. Prefer `slices.Collect()`, `maps.Keys()`, `maps.Values()` over manual loops.
- (Go 1.24+) Prefer `strings.SplitSeq` or `strings.FieldsSeq` in for-range to avoid allocating intermediate slices.

### Data & Allocation

- Use `make` only for slices, maps, and channels.
- Use composite literals (`Point{X: 1, Y: 2}`) to initialize structs.
- (Go 1.18+) Use `any` instead of `interface{}`.
- (Go 1.21+) Use built-in `min(a, b)`, `max(a, b)`, and `clear(mapOrSlice)`.
- (Go 1.26+) Use `new(value)` (e.g., `new(42)`) to get a pointer to a primitive instead of the two-line `v := 42; ptr := &v` boilerplate.
- Enums: custom type (`type Status int`), const block with `iota`, and `//go:generate stringer -type=Status`.

### Generics vs. Interfaces

- Keep interfaces small for composition and duck typing.
- (Go 1.18+) Use type parameters (generics) for general-purpose abstractions instead of `interface{}` and reflection.
- Never write custom sorting/searching loops for slices or maps. Use `slices.Contains`, `slices.SortFunc`, `slices.Compact`, `maps.Clone`, etc.

### Error Handling

- Return errors as values. Reserve `panic` for unrecoverable states. Use `recover` only at API boundaries.
- (Go 1.13+) Wrap errors with context: `fmt.Errorf("... %w", err)`.
- (Go 1.13+) Use `errors.Is(err, target)` instead of `err == target` to match wrapped sentinels like `io.EOF`.
- (Go 1.20+) Use `errors.Join(err1, err2)` to combine multiple errors.
- (Go 1.26+) Use `errors.AsType[T](err)` (type-safe) instead of standard type assertions for extracting concrete error types. Fall back to `errors.As(err, &target)` for older code.

### Concurrency & Context

- Share memory by communicating (channels), not by communicating through shared memory.
- Never store `context.Context` in a struct. Pass it as the first parameter to any function doing I/O, network calls, or blocking ops.
- (Go 1.20+) Use `context.WithCancelCause` for cancellations with specific error reasons.
- `sync.Mutex` is perfectly idiomatic for simple state protection; don't force channels where a mutex is cleaner.
- Use `golang.org/x/sync/errgroup` or `sync.WaitGroup` to ensure all goroutines finish before exit.

### Logging

- (Go 1.21+) Use `log/slog` for structured logging instead of `log.Printf`. Call `slog.Info("msg", "key", value)` with key-value pairs.
- Set a default handler (`slog.SetDefault`) early in `main`.
- On hot paths, prefer `slog.LogAttrs` to avoid allocation from the variadic `any` args.

### HTTP Routing

- (Go 1.22+) The standard `http.ServeMux` now supports method and path parameters: `http.HandleFunc("GET /users/{id}", handler)`. This eliminates the need for lightweight routers like chi or gorilla/mux for simple APIs.

### Project Architecture & Tooling

- `cmd/` for entry points, `internal/` for private code. Only use `pkg/` or root for public library code.
- GOPATH is obsolete. Use `go.mod`. For multi-module local dev, use Go workspaces (`go.work`) instead of `replace` directives.
- (Go 1.21+) Use the `toolchain` directive in `go.mod` to pin the exact Go version for builds. Set both `go` (minimum) and `toolchain` (exact) lines.
- (Go 1.24+) Use `omitzero` struct tag instead of `omitempty` for safer JSON serialization of `time.Time`, `time.Duration`, nested structs/slices/maps.
- (Go 1.20+) Use `strings.Clone(s)` or `bytes.Clone(b)` to prevent memory leaks from large backing arrays.
- (Go 1.21+, stable 1.22) Profile-guided optimization: place a `default.pgo` file (CPU profile from production) in your main package directory and `go build` picks it up automatically.
- Use `golangci-lint` in CI/CD for variable shadowing, unhandled errors, cyclomatic complexity.
- Run `govulncheck ./...` in CI to check dependencies against the Go vulnerability database. Only flags vulns in code paths you actually call.
- Always commit `go.sum`. Run `go mod tidy` before committing. Use `go mod verify` in CI to detect tampered dependencies.
- (Go 1.26) Run `go fix` periodically to auto-migrate legacy code to modern idioms.

### Testing & Quality

#### Table-Driven Tests

Use a slice of anonymous structs with `t.Run()`:

```go
tests := []struct {
    name     string
    input    string
    expected int
}{...}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) { ... })
}
```

For complex struct comparisons, use `github.com/google/go-cmp/cmp` instead of `reflect.DeepEqual`.

#### Test Isolation

- Prefer `t.Cleanup()` over `defer` for mock/resource teardown.
- Use `t.Setenv()` to modify env vars safely without affecting parallel tests.

#### Black-Box Testing

Change your test file's package to `package foo_test` to test only the exported API. Forces you to test behavior, not implementation.

#### Fuzz Testing (Go 1.18+)

```go
func FuzzDivide(f *testing.F) {
    f.Add(10.0, 2.0)
    f.Fuzz(func(t *testing.T, a, b float64) {
        Divide(a, b)
    })
}
```

#### Skipping Slow Tests

```go
if testing.Short() {
    t.Skip("Skipping DB test in short mode")
}
```

Run `go test -short ./...` to skip integration tests during local dev.

#### Parallel Tests & Race Detection

Call `t.Parallel()` at the top of tests/sub-tests. Always run with `go test -race ./...` for concurrent code.

#### Interface-Based Mocking

No monkey-patching in Go. Use dependency injection via interfaces:

```go
type Mailer interface {
    Send(msg string) error
}

func NotifyUser(m Mailer, msg string) { m.Send(msg) }

// In tests:
type MockMailer struct{}
func (m MockMailer) Send(msg string) error { return nil }
```

#### Build Tags for Integration Tests

```go
//go:build integration

package mypkg
```

Run with `go test -tags=integration ./...`. Ignored by default `go test ./...`.
