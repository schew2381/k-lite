# The klite CLI

`klite` is the only client the cluster needs. It speaks gRPC to any `klited` replica over TLS, fails over across a comma-separated server list, and prints tables unless you ask for `-o json` or `-o yaml`.

## Connecting and authenticating

The CLI resolves its target in order:

1. the `--server` flag
2. `KLITE_SERVER`
3. `~/.klite/config`
4. `127.0.0.1:7443`

If you give it several addresses (`--server 127.0.0.1:7443,127.0.0.1:7445`), it round-robins with automatic failover, so a dead klited replica costs you nothing.

Every call authenticates with the admin bearer token, resolved from `KLITE_TOKEN` or `~/.klite/server/token` (minted at klited's first boot and logged once). TLS verifies against the cluster CA from `KLITE_CA` or `~/.klite/server/tls/ca.crt`. On a machine without the CA file, `--insecure` skips server verification while keeping encryption, which is fine for a quick poke and wrong for anything lasting.

On the machine that runs the control plane, none of this needs configuring. The defaults find everything.

## Kinds

Commands that take a `<kind>` accept singular, plural, or lowercase forms of:

- `workloads`
- `instances`
- `services`
- `nodes`
- `networkpolicies`
- `vipallocations` and `ingressallocations`, which the server materializes itself and you can read but never apply

Vocabulary follows [CONTEXT.md](../CONTEXT.md): you declare a Workload, and each Instance is one running copy the server materializes from it.

## Commands

### klite apply

Create or update objects from YAML.

```
klite apply -f examples/apps/b-whoami.yaml
klite apply -f examples/       # a directory, every YAML in it
cat spec.yaml | klite apply -f -
```

Multi-document files work, and each document reports its own result as `created`, `updated`, or `unchanged`. Applying an Instance, VIPAllocation, or IngressAllocation is rejected, since the server materializes those itself.

### klite get

List objects, or stream changes live.

```
klite get workloads
klite get instances -o json    # table is the default; json and yaml for machines
klite get instances --watch    # streams ADDED/MODIFIED/DELETED events until interrupted
klite get nodes
```

`get <kind> <name>` narrows to one object, and `--watch` is the CLI view of the same event stream the web board rides.

### klite describe

`describe` shows one object's spec and status in full, including the scheduler's reason when an Instance sits Pending and the drain state during Node operations.

```
klite describe workload b
klite describe instance b-57d2
klite describe node node-2
```

### klite scale

Set how many Instances a Workload runs.

```
klite scale workload b --replicas 5
```

Scaling down drains the victims (newest first) before removing them, so a running `get instances --watch` shows each one go Draining before it disappears. Scaling to zero keeps the Workload object. Delete it when you mean delete.

### klite logs

Container output, tailed or followed.

```
klite logs b-57d2 --tail 20
klite logs -f a-5877           # follow until Ctrl-C; ends cleanly if the container dies
```

### klite delete

Delete by file or by kind and name.

```
klite delete -f examples/policies/deny-a-to-c.yaml
klite delete workload b
klite delete node node-2       # cordons, drains, then removes, same as deleting its YAML
```

Deleting a Node returns before the record disappears: the controllers finish the drain first (ADR 0033).

### klite drain and klite uncordon

Node maintenance, surge-first.

```
klite drain node-2             # cordon + surge replacements elsewhere + drain, streamed live
klite drain node-2 --force     # delete draining instances immediately instead of waiting out drain timeouts
klite uncordon node-2          # reopen a drained node for scheduling
```

Drain streams its progress in the Nomad style (`cordoned node-2`, `surged b-x1f2 to node-1`, `draining b-9a3c (30s)`, `done`). The cordon survives the drain on purpose. `uncordon` is the explicit way back, and it refuses while a delete is pending.

### klite policy check

Ask before you block.

```
klite policy check a c
```

`policy check` prints the verdict with the matching policy and rule (`denied by deny-a-to-c rule 1`), and it exits nonzero on denied so scripts can gate on it. The data plane enforces the same pure evaluator, so this answer and reality never disagree, and verify-m6 holds them to that.

### klite node token

Mint a join credential.

```
klite node token
```

`node token` prints the `K10<ca-hash>::node:<secret>` token a new node's agent presents once. The hash pins the cluster CA, so the joining machine needs no pre-shared files: apply the node's YAML and run the agent with `--token`. `node add` below declares the node, mints the token, and prints the command to paste.

### klite node add

Declare a node and get its join command in one step.

```
klite node add node-4
klite node add node-4 --labels zone=sfo --max-instances 16
klite node add node-4 --url 203.0.113.7:7443
```

`node add` applies the Node object, mints a join token, and prints two paste-ready blocks. The first is the one-command join for a fresh Linux box, which fetches `join.sh` from the latest GitHub release and leaves the agent running under systemd. The second is the manual fallback for machines the releases don't cover yet: copy `bin/klite-agent` over and run it with `--server`, `--token`, and `--advertise-address`. Re-running the command is safe. Apply reports the node unchanged and the join block prints again.

The printed join line dials `--url`, which defaults to this CLI's own server endpoint. When that endpoint is loopback the output flags it and asks for an address the new machine can reach, one a klited actually listens on (`bin/klited --listen 0.0.0.0:7443`).

On a real Linux machine `--advertise-address` is not optional. The agent's default resolves to the Docker bridge gateway there, usually `172.17.0.1`, and advertising that makes every other node dial its own bridge. `join.sh` detects the machine's public IPv4 and refuses to guess when it only finds private addresses. The full real-machine story, firewall step included, lives in [real-nodes.md](real-nodes.md).

## The knobs behind the commands

| Env / file | Meaning |
| --- | --- |
| `KLITE_SERVER` | klited address list, comma-separated |
| `KLITE_TOKEN` | admin bearer token |
| `KLITE_CA` | cluster CA certificate path |
| `~/.klite/config` | YAML fallback for the server list |
| `~/.klite/server/` | the control plane's own token and TLS material, minted at first boot |
| `~/.klite/agent/<node>/tls/` | a node's mTLS identity, issued at join |
