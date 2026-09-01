# The playground serves the board

The board was a hand-started afterthought. demo.sh's finale launched a facade and a Vite dev server as its last act, and dev-up.sh never started a UI at all. The user asked for the obvious contract instead, one command that brings up the cluster *and* the way to look at it. The frontend side made this cheap: `make ui` now builds a dist that boots straight into live mode at the bare URL, and `klite-facade --ui-dir` serves that dist and its API as one process. So dev-up and demo both build `bin/klite-facade` and the dist alongside the other binaries, start one loopback-bound facade on :7080, and print the board URL. Vite is retired from every scripted flow.

## Considered Options

1. **Keep the Vite finale.** That keeps two processes, a dev-mode CORS hole, and a second gated port. The served bundle is whatever the dev server compiles rather than the dist a self-hoster gets, so the demo was exercising a path nobody ships.
2. **One facade serving the built dist (chosen).** Every playground boot now exercises the same artifact the release tarball carries (ADR 0038), served the same way a self-hoster serves it.
3. **Bind the facade beyond loopback so a LAN laptop can browse the board.** Declined: the facade holds the admin token and has no auth of its own, so loopback *is* its trust boundary (ADR 0040). LAN viewing stays behind the frontend's explicit `bun run dev:lan`, where the exposure is a choice with a name.

## Consequences

- The demo hard-requires bun, since a presentation without the board is broken by definition. dev-up degrades instead, printing a note and bringing the cluster up headless when bun is missing or the UI build fails.
- Board work loses hot reload in the scripted flows. That trade is deliberate, since the playground is for using the board while `dev:lan` remains for building it.
- Port 5173 stops being gated or killed by the demo, so a frontend dev server can run beside a demo without either touching the other.
- dev-down already swept `facade.pid` and port 7080, so teardown needed no change.
