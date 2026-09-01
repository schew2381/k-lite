# klite-net ships as a single static binary. The netns tooling it needs is
# all syscalls, so scratch works. `make net-image` builds it for the local
# daemon's arch. The release workflow builds both linux arches through buildx,
# which sets TARGETARCH per platform (ADR 0038).
FROM scratch
ARG TARGETARCH
ARG BINDIR=bin
COPY ${BINDIR}/klite-net-linux-${TARGETARCH} /klite-net
ENTRYPOINT ["/klite-net"]
