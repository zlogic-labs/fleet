#!/usr/bin/env bash
# Install the Fleet processes as systemd services inside WSL.
#
# Why systemd and not nohup: nothing holds a WSL distribution open once its
# last foreground process exits, so a backgrounded process started from a
# shell dies with that shell, and k3s goes down with it. A systemd unit is
# what keeps the instance — and therefore the cluster and the gateway — alive
# between terminals. This is a development arrangement; a real installation
# runs the same binaries as containers.
set -euo pipefail

ROOT=${ROOT:-/mnt/d/Workspace/zlogic-fleet}
# The Kubernetes controller lives in its own repository, zlogic-labs/fleet-serving.
SERVING=${FLEET_SERVING_DIR:-$ROOT/../fleet-serving}
FLEET_HOME=${FLEET_HOME:-/root/fleet}
NS=${NS:-fleet}
UNIT_DIR=/etc/systemd/system
SKIP_OPERATOR=

say() { printf '\n== %s\n' "$1"; }

say "waiting for k3s"
for _ in $(seq 1 60); do
  if kubectl get nodes >/dev/null 2>&1; then break; fi
  sleep 2
done
kubectl get nodes >/dev/null 2>&1 || { echo "k3s never became ready"; exit 1; }

say "building the binaries"
mkdir -p "$FLEET_HOME/bin" "$FLEET_HOME/state"
for cmd in fleet-gateway fleet-apiserver; do
  (cd "$ROOT/core" && CGO_ENABLED=0 go build -o "$FLEET_HOME/bin/$cmd" "./cmd/$cmd")
done
# fleet-serving is a separate checkout. Skipping it rather than failing keeps
# the gateway and the control plane installable on a host that only runs them.
if [ -d "$SERVING" ]; then
  (cd "$SERVING" && CGO_ENABLED=0 go build -o "$FLEET_HOME/bin/fleet-operator" ./cmd/manager)
else
  echo "  fleet-serving not found at $SERVING; skipping fleet-operator"
  SKIP_OPERATOR=1
fi

# The gateway's upstream is the Service the operator rendered. Its ClusterIP is
# not known until something is deployed, and it changes if the Service is
# recreated, so it is resolved at every start rather than baked into a unit.
# Resolving it into a file the unit reads keeps the gateway's own environment
# free of a shell that would otherwise have to be exec'd through.
say "writing the upstream resolver"
cat > "$FLEET_HOME/upstream.sh" <<EOF
#!/bin/sh
# Regenerates the gateway's upstream from the Service the operator rendered.
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

# The API server is not necessarily up when this runs. A oneshot that exits
# non-zero takes the gateway down with it through Requires=, so a cluster
# that is still starting produces a gateway that never starts.
i=0
while [ \$i -lt 60 ]; do
  kubectl get nodes >/dev/null 2>&1 && break
  i=\$((i+1)); sleep 2
done

svc=\$(kubectl get svc -n $NS -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
model=\$(kubectl get fleetdeployment -n $NS -o jsonpath='{.items[0].status.selector}' 2>/dev/null)
if [ -z "\$svc" ] || [ -z "\$model" ]; then
  # Nothing deployed yet. Writing an empty upstream leaves the gateway serving
  # the console with an empty model list, which is a state an operator can act
  # on. Failing here leaves the gateway not running, which they cannot.
  echo "no FleetDeployment in $NS yet; gateway will serve an empty model list" >&2
  # Bound to loopback, not 0.0.0.0. WSL's localhost forwarding on this host
  # only forwards a service bound to 127.0.0.1, so a wildcard bind is
  # reachable at the WSL address but not from a browser on Windows, which
  # looks exactly like the server being down.
  cat > "$FLEET_HOME/state/upstream.env" <<ENV
FLEET_LISTEN=127.0.0.1:8080
FLEET_UPSTREAMS=
ENV
  exit 0
fi
ip=\$(kubectl get svc "\$svc" -n $NS -o jsonpath='{.spec.clusterIP}')
echo "upstream: \$model -> http://\$ip:8000 (service \$svc)" >&2
cat > "$FLEET_HOME/state/upstream.env" <<ENV
FLEET_LISTEN=127.0.0.1:8080
FLEET_UPSTREAMS=model=\$model,url=http://\$ip:8000,id=\$svc,engine=llama-cpp,replicas=1
ENV
EOF
chmod +x "$FLEET_HOME/upstream.sh"

say "installing units"
cat > "$UNIT_DIR/fleet-upstream.service" <<EOF
[Unit]
Description=Resolve the Fleet gateway's upstream from k3s
Before=fleet-gateway.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$FLEET_HOME/upstream.sh
EOF

cat > "$UNIT_DIR/fleet-gateway.service" <<EOF
[Unit]
Description=Fleet gateway (OpenAI-compatible API and console)
After=fleet-upstream.service
Requires=fleet-upstream.service

[Service]
EnvironmentFile=$FLEET_HOME/state/upstream.env
ExecStart=$FLEET_HOME/bin/fleet-gateway
# Always, not on-failure: a server that exits cleanly without being asked has
# stopped serving, and treating exit 0 as success leaves the gateway down with
# no indication of why.
Restart=always
RestartSec=2
WorkingDirectory=$ROOT

[Install]
WantedBy=multi-user.target
EOF

cat > "$UNIT_DIR/fleet-apiserver.service" <<EOF
[Unit]
Description=Fleet control plane
After=k3s.service

[Service]
ExecStart=$FLEET_HOME/bin/fleet-apiserver --listen 127.0.0.1:8081 \\
  --data $FLEET_HOME/weights --hub stub
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

if [ -z "$SKIP_OPERATOR" ]; then
cat > "$UNIT_DIR/fleet-operator.service" <<EOF
[Unit]
Description=Fleet operator (reconciles FleetModel and FleetDeployment)
After=k3s.service

[Service]
Environment=KUBECONFIG=/etc/rancher/k3s/k3s.yaml
ExecStart=$FLEET_HOME/bin/fleet-operator \\
  --metrics-bind-address :9090 --health-probe-bind-address :9091 --namespace $NS
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
fi

systemctl daemon-reload
systemctl enable fleet-apiserver fleet-upstream fleet-gateway >/dev/null
systemctl restart fleet-apiserver
UNITS="fleet-apiserver fleet-gateway"
if [ -z "$SKIP_OPERATOR" ]; then
  systemctl enable fleet-operator >/dev/null
  systemctl restart fleet-operator
  UNITS="$UNITS fleet-operator"
fi

if [ -z "$SKIP_OPERATOR" ]; then
  say "waiting for a deployment to exist"
  for _ in $(seq 1 90); do
    kubectl get fleetdeployment -n "$NS" >/dev/null 2>&1 && break
    sleep 2
  done
  # The upstream oneshot is RemainAfterExit, so restarting the gateway does not
  # re-run it and the gateway would keep whatever address was resolved last.
  systemctl restart fleet-upstream
fi
systemctl restart fleet-gateway

say "status"
for u in $UNITS; do
  printf '  %-18s %s\n' "$u" "$(systemctl is-active "$u" 2>/dev/null || true)"
done
printf '\n  console  http://127.0.0.1:8080\n'