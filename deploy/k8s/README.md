# Kubernetes deployment

One kustomize base for the whole stack: PostgreSQL, ClickHouse, the gateway,
the control plane and the operator, in the `fleet` namespace. The Kubernetes
controller itself — the CRDs and the deployment they reconcile — lives in
[fleet-serving](https://github.com/zlogic-labs/fleet-serving); its CRDs are
applied from that checkout.

## Prerequisites

- k3s (or any Kubernetes with `kubectl`, which carries kustomize since v1.14)
- Go and node/npm on the machine building the images
- a sibling checkout of `fleet-serving` (override with `FLEET_SERVING_DIR`)

## Install

```sh
./deploy/k8s/build-images.sh   # build the three binaries, import scratch images
./deploy/k8s/install.sh        # CRDs, credentials, kubectl apply -k, wait
```

`install.sh` is idempotent: run it again after a manifest change to converge.

## What is in git and what is not

The manifests reference two objects that `install.sh` generates:

| Object | Contains | Why not in git |
|---|---|---|
| Secret `fleet-env` | admin token, database and ClickHouse passwords, `DATABASE_URL` | credentials |
| ConfigMap `fleet-urls` | this host's addresses, ClickHouse endpoint | host-specific |

Both are reused across runs, so redeploying does not invalidate a token the
console is already holding. Changing this host's IP is picked up on the next
`install.sh` — it restarts the deployments, because env from a ConfigMap is
resolved once, when the container starts.

## Ports

The gateway (8080) and the control plane (8081) are Services of type
`LoadBalancer`, served by ServiceLB (klipper-lb) — bundled with k3s, the same
mechanism already exposing Traefik, so there is nothing extra to install. The
console reads one control-plane URL out of `/fleet/status` and the browser
fetches it directly, so the number has to stay 8081 either way.

`hostPort` was tried first and is why this section exists: a pod holding its
own port deadlocks its rolling update — the old pod keeps 8081 until the
replacement is Ready, the replacement cannot schedule onto a node whose port
is taken, and nothing can move. Exposure belongs to a service; a workload
should not own a node port.

`install.sh` prints the console URL and the control-plane token at the end.
The token goes into the console's Settings drawer; it is stored in that
browser only.

## Host routing

If the host runs a proxy with TUN auto-route (mihomo, clash-meta), it grabs
every DNS packet on the machine — including the pod network's queries to
CoreDNS. Names then resolve into the proxy's fake-address range and the whole
cluster loses name resolution, which presents as "postgres: ping: context
deadline exceeded". `install.sh` runs `cluster-routes.sh`, which pins the pod
and service networks to the main routing table at a priority the proxy does
not own. It is additive (the proxy's config is untouched), idempotent, and a
no-op on a host without such a proxy.

## After changing code

```sh
./deploy/k8s/build-images.sh && kubectl -n fleet rollout restart deploy
```

Images are tagged `:dev` and never pulled from a registry: they are imported
into the node's containerd directly, which is why there is no Dockerfile here
and no registry to authenticate to.

## Notes

- **Cluster name.** The operator reports the cluster as `k3s-dev`
  (`operator.yaml`, `--cluster-name`). It lands in `usage_events` and the cost
  pool, so change it there if this is not a development cluster.
- **Schema.** The control plane migrates at startup (`IF NOT EXISTS` only);
  the gateway is deliberately not given DDL privileges.
- **ClickHouse** is created by the gateway on its first start — the detail
  replica is its to migrate. Until it is up, the control plane's
  reconciliation page reports the replica as unavailable, which is the truth.
- **Object storage.** No S3 endpoint is configured, so pulled weights go to
  the `fleet-data` PVC under `--data`. Point `FLEET_S3_*` at a bucket to
  change that.
- **Password rotation.** The generated passwords are reused, but PostgreSQL
  honours `POSTGRES_PASSWORD` only when it initialises the data directory.
  Rotating one on a live volume needs `ALTER USER` as well.

## Verify

```sh
kubectl -n fleet get pods
curl -s http://<node>:8080/healthz          # gateway
curl -s http://<node>:8081/healthz          # control plane
curl -s -o /dev/null -w '%{http_code}\n' \
  http://<node>:8081/api/v1/tenants         # 401 without a token
kubectl -n fleet exec statefulset/fleet-postgres -- \
  psql -U fleet -d fleet -c '\dt'           # the ledger tables
```
