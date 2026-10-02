# The inventory contract

The only interface between [fleet](this repository) and
[fleet-serving](https://github.com/zlogic-labs/fleet-serving).

fleet-serving reads Kubernetes because it is the only component allowed to.
It pushes what it finds; the control plane stores it, and the gateway learns
its endpoints from the same records. Nothing calls back into the operator, and
neither side ever learns what produced a report.

```
PUT {control-plane}/api/v1/inventory
Content-Type: application/json
Authorization: Bearer …        # only when the control plane requires a token

204 No Content
```

`PUT` rather than `POST`: the body is the whole state of one cluster, so
reporting twice must leave the same state rather than two copies.

## Body

`core/pkg/inventory` — [`Report`](../core/pkg/inventory/inventory.go). Both
repositories compile against this package, so a type change breaks both builds
at once rather than shipping a field one side silently drops.

| Field | Meaning |
|---|---|
| `contract` | The sender's contract version. Absent means the sender predates it. |
| `cluster.name` | Identifies the cluster. Two operators may report to one control plane without overwriting each other. Required. |
| `cluster.version` | Kubernetes version, as the node reports it. |
| `cluster.reachable` | Whether the operator could see the API server. |
| `cluster.nodeCount` / `readyNodes` | Recomputed by the control plane on receipt, so the summary cannot disagree with the node list. |
| `cluster.gpuCount` / `readyGpus` | As above. `readyGpus` counts GPUs on ready nodes, not ready GPUs. |
| `cluster.cpuMillicores` / `memoryMiB` | Cluster-wide allocatable totals. |
| `cluster.nodes[]` | One per node. |
| `cluster.reportedAt` | Stamped by the **control plane** on receipt if the operator left it zero. |
| `cluster.message` | Free text, typically an error an operator can act on. |
| `deployments[]` | One per `FleetDeployment`, with `namespace` + `name` as the key. Both required. |
| `deployments[].address` | The in-cluster endpoint the controller rendered, `host:port`. A deployment without one is a promise the operator has made and not yet kept, and the gateway will not route to it. |
| `deployments[].state` | `Pending`, `Scheduling`, `InsufficientCapacity`, `Progressing`, `Available`, `Degraded`, `Failed`. |
| `deployments[].reason` | Why, when the state is not `Available`. |
| `deployments[].selector` | The model name clients address this deployment by. |
| `deployments[].engine` / `version` / `format` | What was asked for, what answered, and what the weights turned out to be. `version` is proof a real runtime answered rather than an assumed value. |
| `deployments[].updatedAt` | Stamped by the **control plane** on receipt: how long ago the fact arrived, which is what "is this still true" means. |

`Pending` and `InsufficientCapacity` are separate states because they need
different actions. From outside a `Deployment`'s status they look identical:
one resolves on its own, the other needs the spec changed.

## Changing it

Within v1:

- **Fields may be added.** They must be optional, and omitted when the sender
  does not know them, so a newer operator can report to an older control plane.
- **Fields may not be renamed, retyped or removed.** A rename is a new contract
  wearing the old version number. Both sides compile against the same package,
  so the compiler catches it before a console cell renders empty.
- **Semantics may not change** without bumping the version.

Receivers must ignore fields they do not recognise. That direction has to work:
during a rolling upgrade the operator goes first about half the time, and a
receiver that rejected unknown fields would take a whole cluster's inventory
down until both halves were upgraded.

A version bump touches three places, and `core/internal/apiserver` panics at
startup if the first two are changed without the third:

1. `ContractVersion` in `core/pkg/inventory/contract.go`
2. `Path` in the same file — the operator sends to this constant
3. `apiPrefix` in `core/internal/apiserver/server.go`

## The golden

`core/pkg/inventory/testdata/report.v1.json` is a fully-populated report and is
the normative answer to "what does v1 look like". Two tests hold it in place,
one per repository:

- `core/pkg/inventory/contract_test.go` — this build marshals exactly the
  golden, the golden round-trips, and an unknown field is ignored rather than
  rejected.
- `fleet-serving/internal/report/contract_test.go` — what the reporter actually
  puts on the wire is the golden, with the version stamped by the reporter and
  not by its caller.

Neither test asserts against the struct it is testing; that would agree with
itself. Both compare against bytes someone else agreed to.