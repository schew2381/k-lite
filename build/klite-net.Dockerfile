# klite-net ships as a single static binary. The netns tooling it needs is
# all syscalls, so scratch works. `make net-image` builds it.
FROM scratch
COPY bin/klite-net-linux-arm64 /klite-net
ENTRYPOINT ["/klite-net"]
