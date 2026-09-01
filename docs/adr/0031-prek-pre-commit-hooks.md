# prek runs staged-only commit hooks that never rewrite Go

Several sessions commit to `main` in parallel, and the Go ones carry long-lived uncommitted edits while the frontend rides on bun and Biome. Nothing checked a commit at the moment it happened. The repo did carry a `prek.toml`, though only `make precommit` ever ran it, and its hook set targeted full repo sweeps rather than commits. It rewrote files as it checked them and ran the full linter and test suite on every invocation. Turning that on as a live hook would reformat files out from under whichever session was mid-edit. prek now manages the pre-commit hook, with the same `prek.toml` carrying a new fast hook set. Each clone turns it on once with `prek install`. Every hook receives staged paths only and finishes in about a second:

- Biome checks and fixes `frontend/` sources through bun from that directory
- the gofmt hook fails with a file list and never writes
- hygiene guards come from the standard pre-commit-hooks set

## Considered Options

1. **pre-commit (Python).** It reads the same config and the hook ecosystem grew up around it. It also drags a Python environment into a repo whose toolchains are Go and bun, and it starts slower than everything else on this list.
2. **husky + lint-staged.** Both live in npm, which suits a frontend-only repo. Here they'd move the whole repo's hook policy into `frontend/package.json` and hang the Go-side checks on a JavaScript install being present.
3. **lefthook.** It ships as one fast binary with its own YAML dialect. It can't consume standard pre-commit hook repos, so the hygiene checks would become scripts we maintain ourselves.
4. **prek with the standard `.pre-commit-config.yaml`.** The YAML is the format the wider hook ecosystem documents, and any pre-commit-compatible runner could take over without a config rewrite. prek prefers a `prek.toml` when both files exist though, so a stray TOML hijacks the hook and nobody notices.
5. **prek with its native `prek.toml` and a rewritten hook set** (chosen). One tool reads one file that nothing can shadow, since the TOML always wins the precedence race. The config still pins the upstream hygiene hooks at a released rev, and the fast hooks land as a content change to a file the repo already tracks. We give up portability to other runners, which costs little because nothing here runs anything except prek.

## Consequences

- Hooks act only on the paths git has staged, but prek doesn't leave unstaged edits in place during a run. It saves them to a patch, resets the working tree to the index for the hook window, and reapplies the patch when hooks finish. A write from any other process during that window reads as a hook edit and fails the commit. If the write lands on a file the patch covers, the restore then reverts it, and the new content survives nowhere. We verified the whole cycle in a sandbox against prek 0.5.0 and its upstream source, and the Python pre-commit behaves identically.
- A checkout with a single session never notices that cycle, so `prek install` belongs in single-session clones only. The shared checkout keeps the hook uninstalled and runs the same set through `make precommit`, which uses `--all-files` and never stashes. Bare `prek run` does stash, so the make target is the only sanctioned manual entry point there. Nobody waits on a test suite to land a one-line change either way, since the fast set stays under a second.
- Go stays check-only. The gofmt hook prints the files that need formatting and fails. A human applies the fix, and the hook never fights gofumpt or an editor mid-save.
- The config stays in `prek.toml` at the repo root and now carries the fast hook set. `make precommit` runs that same set, and the heavier gates keep their own make targets.
- check-yaml runs with multi-document support because the example manifests separate kinds with `---`.
- CI stays the full gate. Everything the hook skips still runs there on every push.
- Editor defaults travel with the repo. `.editorconfig` and `.vscode/` carry the same Biome-first settings the hook enforces, and `bun run check` gives humans the fuller frontend gate in one command.
- New contributors run `brew install prek` once and `prek install` once per clone.
