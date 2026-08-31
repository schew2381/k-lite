# k-lite

k-lite is a small Kubernetes-like orchestrator built from scratch to understand how the real one works. Declarative YAML goes in, scheduled containers come out, and service discovery and network policies run on top of plain Docker.

## Language

**Workload**:
A named set of identical Instances run from one container image, plus how many of them to run.
_Avoid_: Deployment, app

**Instance**:
One running copy of a Workload, scheduled to a Node and backed by exactly one container.
_Avoid_: Pod, replica

**Service**:
A stable name and port that routes to the ready Instances selected by a label.

**VIP**:
The virtual IP a Service resolves to on a given Node. Each Node's proxy owns its own VIP per Service, and it stays fixed for the Service's lifetime.
_Avoid_: ClusterIP

**Node**:
A machine that runs Instances. Today each Node is simulated by one local agent process, and later it's real hardware running the same agent.
_Avoid_: host, worker

**NetworkPolicy**:
A named set of ALLOW or DENY rules between Services. DENY always wins, and a Service no ALLOW rule targets accepts everyone.
_Avoid_: ACL, firewall rule

**Endpoint**:
One Instance viewed as a routing target, carrying a readiness state.
_Avoid_: backend

**Draining**:
The state in which an Endpoint or Node takes no new connections while existing ones finish.

**Cordon**:
Marking a Node unschedulable without disturbing what already runs there.

**Join token**:
The one-time secret a Node presents to enter the cluster and receive its identity certificate.

**Infra pod**:
Two per-Node system containers that share one network namespace and give that Node's Instances name resolution and traffic routing. The k8s word is borrowed on purpose, because this pair is exactly what Kubernetes calls a pod.
