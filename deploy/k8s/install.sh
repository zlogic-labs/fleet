#!/usr/bin/env bash
# Deploy Fleet onto a k3s cluster: the operator's CRDs, the two objects whose
# values must never enter git, then the kustomize base. Idempotent — run it
# again to converge after a manifest or image change.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
SERVING=${FLEET_SERVING_DIR:-$ROOT/../fleet-serving}
NS=${NS:-fleet}
NODE_IP=${NODE_IP:-$(hostname -I | awk '{print $1}')}

say() { printf '\n== %s\n' "$1"; }

say "waiting for the cluster"
for _ in $(seq 1 60); do kubectl get nodes >/dev/null 2>&1 && break; sleep 2; done
kubectl get nodes >/dev/null 2>&1 || { echo "k3s never became ready"; exit 1; }

# Without this, a proxy with TUN auto-route answers the pod network's DNS
# queries itself and every name in the cluster resolves into the proxy's
# fake-address range. The failure presents as a database timeout, so it is
# cheaper to prevent than to diagnose.
say "keeping cluster DNS off the host's proxy TUN"
bash "$ROOT/deploy/k8s/cluster-routes.sh"

say "installing the operator's CRDs"
kubectl apply -k "$SERVING/config/crd"
kubectl wait --for=condition=Established \
  crd/fleetmodels.fleet.zlogic.com crd/fleetdeployments.fleet.zlogic.com --timeout=60s

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -

say "providing the values the manifests cannot carry"
# Reuse what is already stored, so a redeploy does not invalidate the token
# the console is holding. Generate only what is missing.
if kubectl -n "$NS" get secret fleet-env >/dev/null 2>&1; then
  get() { kubectl -n "$NS" get secret fleet-env -o jsonpath="{.data.$1}" | base64 -d; }
  ADMIN_TOKEN=$(get ADMIN_TOKEN)
  DB_PASS=$(get POSTGRES_PASSWORD)
  CH_PASS=$(get CLICKHOUSE_PASSWORD)
else
  ADMIN_TOKEN=$(head -c 32 /dev/urandom | base64 | tr -d '=+/' | cut -c1-40)
  DB_PASS=$(head -c 24 /dev/urandom | base64 | tr -d '=+/' | cut -c1-24)
  CH_PASS=$(head -c 24 /dev/urandom | base64 | tr -d '=+/' | cut -c1-24)
fi

# The gateway and the console read the control-plane URL from one place. If it
# changes — a new address for this host — the pods must be restarted, because
# env from a ConfigMap is resolved once, at container start.
OLD_CFG=$(kubectl -n "$NS" get cm fleet-urls -o jsonpath='{.metadata.resourceVersion}' 2>/dev/null || echo none)

kubectl -n "$NS" apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: fleet-env
type: Opaque
stringData:
  ADMIN_TOKEN: $ADMIN_TOKEN
  POSTGRES_USER: fleet
  POSTGRES_PASSWORD: $DB_PASS
  POSTGRES_DB: fleet
  DATABASE_URL: postgres://fleet:$DB_PASS@fleet-postgres:5432/fleet?sslmode=disable
  CLICKHOUSE_PASSWORD: $CH_PASS
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: fleet-urls
data:
  FLEET_CONTROL_PLANE_URL: http://$NODE_IP:8081
  FLEET_ALLOWED_ORIGINS: http://$NODE_IP:8080
  FLEET_CLICKHOUSE_URL: http://fleet-clickhouse:8123
  FLEET_CLICKHOUSE_DATABASE: fleet_detail
  FLEET_CLICKHOUSE_USER: fleet
EOF

say "applying the base"
kubectl apply -k "$ROOT/deploy/k8s/base"

NEW_CFG=$(kubectl -n "$NS" get cm fleet-urls -o jsonpath='{.metadata.resourceVersion}' 2>/dev/null || echo none)
if [ "$OLD_CFG" != none ] && [ "$OLD_CFG" != "$NEW_CFG" ]; then
  say "connection details changed; restarting Fleet"
  kubectl -n "$NS" rollout restart deploy/fleet-gateway deploy/fleet-apiserver deploy/fleet-operator
fi

say "waiting for the data services"
kubectl -n "$NS" rollout status statefulset/fleet-postgres --timeout=180s
kubectl -n "$NS" rollout status statefulset/fleet-clickhouse --timeout=600s

say "waiting for Fleet"
kubectl -n "$NS" rollout status deploy/fleet-apiserver --timeout=180s
kubectl -n "$NS" rollout status deploy/fleet-gateway --timeout=180s
kubectl -n "$NS" rollout status deploy/fleet-operator --timeout=180s

printf '\n  console  http://%s:8080\n' "$NODE_IP"
printf '  control  http://%s:8081/api/v1\n' "$NODE_IP"
printf '\n  console token  %s\n' "$ADMIN_TOKEN"
printf '  (Settings -> Control plane token; stored in this browser only)\n'
