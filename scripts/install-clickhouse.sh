#!/bin/bash
# Runs ClickHouse in WSL for Fleet's detail store.
#
# Fleet does not need it to work: Postgres is the authoritative ledger and every
# report can be recomputed from it. This store is a replica for the three query
# shapes that Postgres is the wrong shape for, which is why a failure here must
# never fail a request.
#
# A container on k3s is the only option that does not need a second stateful
# thing installed, and the image is already pulled: MinIO turned out to publish
# no binaries at all, so "install the server directly" is not a path that
# reliably exists for any of these.
set -euo pipefail

NS=fleet-clickhouse
POD=clickhouse
IMAGE=docker.io/clickhouse/clickhouse-server:24.8-alpine
PORT=8123

say() { printf '  %s\n' "$*"; }

if ! command -v kubectl >/dev/null 2>&1; then
  echo "kubectl not found; is k3s running?" >&2
  exit 1
fi

kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS" >/dev/null

# A PVC rather than emptyDir: this data is the whole point of the store, and it
# must survive the pod being rescheduled. local-path is the k3s default.
kubectl -n "$NS" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: clickhouse-data
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 8Gi
---
apiVersion: v1
kind: Service
metadata:
  name: clickhouse
spec:
  selector:
    app: clickhouse
  ports:
    - name: http
      port: $PORT
      targetPort: $PORT
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: clickhouse
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: clickhouse
  template:
    metadata:
      labels:
        app: clickhouse
    spec:
      containers:
        - name: clickhouse
          image: $IMAGE
          ports:
            - containerPort: $PORT
              name: http
              # Bound on the node as well as on the Service. A cluster IP lives
              # in a range k3s does not route out of the node, so without this
              # the store is unreachable from the WSL host -- which is where an
              # operator runs clickhouse-client, and where the Go tests on a
              # Windows checkout connect from.
              hostPort: $PORT
          env:
            # The bundled default user has no password and is only reachable
            # from inside the cluster. A password would be one more secret to
            # rotate; the network boundary is the one that matters here, and
            # this store holds no credential of any kind.
            - name: CLICKHOUSE_USER
              value: fleet
            - name: CLICKHOUSE_PASSWORD
              value: ""
            - name: CLICKHOUSE_DB
              value: fleet_detail
            - name: CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT
              value: "1"
          volumeMounts:
            - name: data
              mountPath: /var/lib/clickhouse
          readinessProbe:
            httpGet:
              path: /ping
              port: $PORT
            initialDelaySeconds: 5
            periodSeconds: 3
          resources:
            requests:
              cpu: 250m
              memory: 1Gi
            limits:
              memory: 2Gi
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: clickhouse-data
YAML

kubectl -n "$NS" rollout status deployment/clickhouse --timeout=180s >/dev/null
IP=$(kubectl -n "$NS" get svc clickhouse -o jsonpath='{.spec.clusterIP}')
NODE=$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
say "clickhouse ready at $IP:$PORT"

# The node address is what goes in the env file, not the cluster IP. The
# ClusterIP is what fleet-gateway uses successfully from inside the cluster, but
# it is not routable from the host, and the host is where an operator and the
# test suite both live.
cat > /root/fleet/state/clickhouse.env <<ENV
FLEET_CLICKHOUSE_URL=http://$NODE:$PORT
FLEET_CLICKHOUSE_DATABASE=fleet_detail
FLEET_CLICKHOUSE_USER=fleet
ENV
chmod 600 /root/fleet/state/clickhouse.env

# The gateway is started by install-systemd.sh with an EnvironmentFile, so
# adding this one is enough for both processes to see it. It is a separate file
# because a control plane with no detail store must still start.
for unit in fleet-gateway fleet-apiserver; do
  if systemctl cat "$unit" 2>/dev/null | grep -q EnvironmentFile; then
    if ! grep -q clickhouse.env <(systemctl cat "$unit"); then
      sed -i "s|^EnvironmentFile=\(.*admin.env\)|EnvironmentFile=\1\nEnvironmentFile=/root/fleet/state/clickhouse.env|" \
        "/etc/systemd/system/$unit.service"
      systemctl daemon-reload
      say "wired $unit to clickhouse.env"
    fi
  fi
done

echo
say "url      http://$IP:$PORT"
say "env file /root/fleet/state/clickhouse.env (mode 600)"
say "uninstall kubectl delete ns $NS"