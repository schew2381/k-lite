# Real machines join from GitHub Releases, and the donor image rides NetBootstrap

Joining a real machine used to mean six manual steps, ending in a hand-written Dockerfile on the box (docs/real-nodes.md). M11 packages the path k3s proved: a tag push builds `klite`, `klited`, `klite-agent`, and `klite-net` for linux/amd64, linux/arm64, and darwin/arm64, attaches them to a GitHub Release next to `hack/join.sh`, and publishes a multi-arch `klite-net` image to `ghcr.io/schew2381/klite-net`. `klite node add <name>` declares the Node, mints its token, and prints the curl-pipe line a new box pastes. The one wire change is `net_image` on NetBootstrap: klited's `--net-image` pins the donor image cluster-wide, and an empty field keeps the agent's compiled-in `klite-net:dev`.

## Considered Options

1. **Clone the repo and `go build` on each node.** Works against a private repo today, and it was the recommended stopgap. Rejected as the permanent path: every node carries a Go toolchain plus repo credentials, and "wipe the box and re-join" stops being one line.
2. **Ship the agent itself as a container.** One artifact for every arch, but it manages the host from inside a container (docker socket and state dir mounted through), adds a layer over what is one static binary, and still needs a public registry, the same gap it was meant to close.
3. **Config management (ansible and friends).** The right shape at fleet scale, three files of ceremony for one box. Nothing stops an operator from wrapping join.sh in it later.
4. **Release binaries plus join.sh plus ghcr** (chosen). A node needs curl and root. The cost is this repo owning release wiring, and the downloads only work once the repo (or at least its releases and packages) is public.

For the image knob, a per-agent `--net-image` flag was the alternative. Rejected because image-version skew across nodes is exactly the drift NetBootstrap exists to prevent: it already carries every cluster-level value nodes must agree on (port bases, infra IPs, cluster id), so the image pin joins them as an additive field.

## Consequences

- Until the repo goes public and a first tag is pushed, join.sh has nothing to download. The script names that cause when its download fails, and `klite node add` prints the copy-the-binary fallback alongside the curl line.
- The empty-means-default field keeps every existing cluster byte-identical: old servers send nothing, dev clusters keep `klite-net:dev`, and agents older than the field ignore it.
- The donor image is part of the donor's config hash, so flipping `--net-image` recreates each infra pod exactly once, with the usual Envoy recreate riding along (ADR 0008).
- Release binaries build with `CGO_ENABLED=0`, pinning the pure-Go constraint: a future cgo dependency would break the cross-build matrix loudly.
- The klite-net Dockerfile's `TARGETARCH` parameterization makes `make net-image` and the buildx release share one recipe, so dev and released images differ only in tag.
- Rootless Docker stays out of scope for join.sh, as it is for the socket probe (docs/real-nodes.md); stock installs from get.docker.com are the supported shape.
