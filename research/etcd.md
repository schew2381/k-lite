# etcd research for ADR 0005 / M1

Sources: a local clone at `~/code/etcd` (main), the etcd.io v3.5 op-guide, and registry manifests
checked 2026-08-31. Topology and failure claims were exercised live on Docker 29.5.2 (linux/arm64).

## What it is

etcd is a raft-replicated key-value store with MVCC. Every write bumps a cluster-wide revision, and
each key carries `create_revision`, `mod_revision`, and `version`. Reads, transactions, and watches
can all be pinned to a revision, so an API server can stay stateless: the store is both source of
truth and version counter.

## The 3-member local topology

Both registries publish the same multi-arch manifests (`scripts/build-docker.sh:50-51` tags both),
and registry checks confirm `gcr.io/etcd-development/etcd:v3.5.33` and `quay.io/coreos/etcd:v3.5.33`
both contain `linux/arm64`. v3.5.33 is the newest 3.5 patch and v3.6.14 the current stable line.
arm64 has been supported since v3.5.0 (`CHANGELOG/CHANGELOG-3.5.md:923`), so `ETCD_UNSUPPORTED_ARCH`
isn't needed (`server/etcdmain/etcd.go:240`).

Peers talk on a Docker network and clients come in through published localhost ports. The flags
follow https://etcd.io/docs/v3.5/op-guide/container/ and were verified by booting this cluster:

```bash
docker network create klite-etcd
# member 1 shown. Member 2 publishes and advertises :2381, and member 3 uses :2383.
docker run -d --name etcd-1 --network klite-etcd \
  -p 127.0.0.1:2379:2379 -v klite-etcd-1:/etcd-data \
  gcr.io/etcd-development/etcd:v3.5.33 \
  /usr/local/bin/etcd --name etcd-1 --data-dir /etcd-data \
  --listen-client-urls http://0.0.0.0:2379 --advertise-client-urls http://127.0.0.1:2379 \
  --listen-peer-urls http://0.0.0.0:2380 --initial-advertise-peer-urls http://etcd-1:2380 \
  --initial-cluster etcd-1=http://etcd-1:2380,etcd-2=http://etcd-2:2380,etcd-3=http://etcd-3:2380 \
  --initial-cluster-state new --initial-cluster-token klite-etcd \
  --auto-compaction-mode=periodic --auto-compaction-retention=30m
```

Details that bite, all hit during the live test:

- The image has been distroless since v3.5.7 (`CHANGELOG/CHANGELOG-3.5.md:505-515`), so it has no
  shell and `docker run --health-cmd` fails (docker wraps that command in `/bin/sh -c`). Health
  checks need exec form, either `docker exec etcd-1 etcdctl endpoint health` or compose's
  `test: ["CMD", "/usr/local/bin/etcdctl", "endpoint", "health"]`. From the host, each member
  answers `curl http://127.0.0.1:{2379,2381,2383}/health`.
- `--advertise-client-urls` is what `MemberList` returns. Advertising the published localhost ports
  serves host clients but breaks `etcdctl endpoint health --cluster` inside a container, which dials
  `127.0.0.1:2381` and reaches its own loopback. In-container checks must list endpoints explicitly.
- `--initial-*` flags apply only to first boot with an empty data-dir and "will be ignored on
  subsequent runs of etcd" (https://etcd.io/docs/v3.5/op-guide/clustering/). Leaving them in place
  is safe, and restarted members rejoined once their volumes were reattached.

## clientv3 patterns k-lite uses

etcd ships a Kubernetes-flavored client whose `OptimisticPut` is the write path k-lite needs, a txn
guarded by compare-on-ModRevision (`client/v3/kubernetes/client.go:85`):

```go
get, _ := cli.Get(ctx, key)   // key "/klite/v1/pods/nginx-1", Kvs[0].ModRevision = resourceVersion
txn, _ := cli.Txn(ctx).
    If(clientv3.Compare(clientv3.ModRevision(key), "=", get.Kvs[0].ModRevision)).
    Then(clientv3.OpPut(key, updated)).
    Else(clientv3.OpGet(key)).                    // fetch current state on conflict
    Commit()
if !txn.Succeeded { /* 409: caller re-reads and retries */ }
```

Creates guard on `clientv3.CreateRevision(key) = 0` instead. List+watch with revision resume
follows `client/v3/mirror/syncer.go:115-120`, listing at revision R and then watching from R+1:

```go
lresp, _ := cli.Get(ctx, "/klite/v1/pods/", clientv3.WithPrefix())
rev := lresp.Header.Revision
wch := cli.Watch(ctx, "/klite/v1/pods/", clientv3.WithPrefix(), clientv3.WithRev(rev+1))
for wr := range wch {
    if wr.Err() != nil { /* ErrCompacted: re-list, rebuild cache, re-watch from new rev+1 */ }
    for _, ev := range wr.Events { apply(ev) }    // ev.Type PUT/DELETE, ev.Kv.ModRevision
}
```

The client survives disconnects on its own, retrying "on other recoverable errors forever until
reconnected" and resuming from the last delivered revision (`client/v3/watch.go:85-86`). Compaction
is the one error left to us: the server cancels the watch and the channel closes with `ErrCompacted`
(`client/v3/watch.go:105-128`), and recovery is always re-list then re-watch.

Key layout `/klite/v1/<kind>/<name>` works as planned. Keys are flat bytes, and `WithPrefix()` turns
a prefix into a range query or watch (`client/v3/op.go:424`), so one watch per kind or one watch on
all of `/klite/v1/` both work. Keep the trailing slash so `pods` can't match `podtemplates`.

## Leader election for controllers

`concurrency.Session` wraps a lease plus a keepalive goroutine, with a default TTL of 60s
(`client/v3/concurrency/session.go:26`) settable via `concurrency.WithTTL(seconds)`.
`Election.Campaign` writes `<prefix>/<leaseID>` bound to that lease via a create-revision-0 txn and
blocks until every lower-revision candidate key is gone (`client/v3/concurrency/election.go:69-107`).

```go
sess, err := concurrency.NewSession(cli, concurrency.WithTTL(10))
e := concurrency.NewElection(sess, "/klite/v1/leader/controller-manager")
if err := e.Campaign(ctx, hostname); err != nil { return err } // blocks until we lead
go runControllers(controllerCtx)
select {
case <-sess.Done(): // lease gone (expired, orphaned, or quorum lost) so stop actuating now
    stopControllers()
case <-shutdown:
    e.Resign(context.Background()) // deletes the key so the next candidate takes over at once
    sess.Close()                   // Orphan then Revoke
}
```

Standbys block in `Campaign`, and `e.Observe(ctx)` streams the leader value. Failure timing:

- On clean shutdown, `Resign` deletes the leader key and takeover happens in milliseconds.
- After a SIGKILL, keepalives stop, etcd expires the lease at the end of its TTL and deletes the
  bound key, and the next candidate unblocks. Takeover latency is bounded by the session TTL, so up
  to 10s with `WithTTL(10)` and 60s at the default.
- If etcd's own raft leader dies at the same moment, the new leader re-extends outstanding leases
  (https://etcd.io/docs/v3.5/op-guide/failures/), so expiry can overshoot by about one more TTL
  (etcd's internal failover itself takes about `--election-timeout`, default 1000ms).
- A paused-but-alive old leader is the split-brain case. It must treat `sess.Done()` as a stop
  signal, and leader-only writes can be fenced with a txn on `e.Key()` and `e.Rev()`.

## Ops gotchas for a small cluster

- Auto-compaction is off by default (`--auto-compaction-retention '0'`,
  `server/embed/config.go:714`), so MVCC history grows until the quota trips. Run with
  `--auto-compaction-mode=periodic --auto-compaction-retention=30m`.
- The backend quota defaults to 2GiB (`--quota-backend-bytes`, `server/embed/config.go:616`).
  Exceeding it raises a `NOSPACE` alarm and degrades the cluster to reads and deletes until you
  compact, defrag, and `etcdctl alarm disarm` (https://etcd.io/docs/v3.5/op-guide/maintenance/).
- Compaction frees no disk space. `etcdctl defrag` does, but it blocks reads and writes on the
  member being defragged, so run one member at a time.
- With one member of three down, quorum holds and the live test showed `put` still succeeding.
- With two members down, quorum is gone. On the survivor, `put` and plain `get` both failed with
  `context deadline exceeded`, while `get --consistency=s` (serializable) still answered from local
  data. Lease expiry can't commit either, so the leader key freezes, the frozen leader's own
  keepalives fail, `sess.Done()` fires, and controllers stop with no successor until quorum
  returns. M7 should assert exactly that, and writes resumed the moment one member restarted.

## The kube-apiserver parallel

kube-apiserver keeps no state of its own: every object lives under `/registry/...` in etcd, so any
replica can serve any request. `resourceVersion` is the etcd revision, and writes are the
compare-ModRevision txn shown above. Each apiserver runs one etcd list+watch per resource into an
in-memory watch cache and serves client GET/LIST/WATCH from it. A client that presents a
`resourceVersion` older than the cache window gets `410 Gone` and must re-list, the same shape as
etcd's `ErrCompacted` (https://kubernetes.io/docs/reference/using-api/api-concepts/). klited copies
all of this, except that Kubernetes controllers elect via a `Lease` object through the apiserver
while k-lite controllers go to etcd's `concurrency.Election` directly.

## Verdict for ADR 0005

The decision holds up, and the risky parts are now tested rather than assumed. ModRevision txns give
optimistic concurrency, prefix watch with revision resume feeds caches and controllers, and
`Session` plus `Election` bounds takeover by a TTL we pick (10s is a sane start). Pin
`gcr.io/etcd-development/etcd:v3.5.33`, pulled and run on arm64 here. M1 must carry over:

- a user Docker network for peers, with three localhost ports published for clients
- named volumes for `--data-dir`, so restarts rejoin instead of re-bootstrapping
- exec-form health checks, because the image has no shell
- auto-compaction on from day one, plus re-list-on-`ErrCompacted` watch recovery in klited
