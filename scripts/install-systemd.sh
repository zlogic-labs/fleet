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
# The name the operator reports the cluster under. It lands in usage_events and
# the cost pool, so it is worth being explicit about rather than letting it
# default to whatever the first node is called.
CLUSTER_NAME=${CLUSTER_NAME:-k3s-dev}
APISERVER=http://127.0.0.1:8081
# The console and the control plane are bound to the wildcard address because
# WSL's localhost forwarding is unreliable here, and a browser on Windows that
# cannot reach the server looks exactly like the server being down. Print the
# WSL address at the end so you know what to open.
#
# Note that a wildcard bind is every interface, not the vEthernet one — the
# earlier comment here said otherwise, and it was wrong. The control plane
# refuses to start on a wildcard bind without an admin token, which is what
# keeps this from being a mistake; the gateway still needs FLEET_API_KEYS set
# before it is reachable by anything but this machine.
WSL_ADDR=${WSL_ADDR:-$(hostname -I | awk '{print $1}')}
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

# Without an operator there is nothing to discover endpoints from, so the
# gateway is pointed at whatever Service the operator last rendered. With one,
# the operator reports the cluster and the gateway reads it, and this whole
# mechanism is skipped: a ClusterIP baked into a file goes stale the moment the
# Service is recreated, and the oneshot holding it never re-runs.
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
# Tenancy, budgets and the cost pool all live in PostgreSQL. Without a URL the
# control plane still runs and the registry pages still work, which is what a
# laptop with no database wants, so this stays optional rather than required.
DATABASE_ARG=
if [ -n "${DATABASE_URL:-}" ]; then
  DATABASE_ARG=" --database $DATABASE_URL"
fi

# The admin token, generated once and reused.
#
# The control plane is bound to the wildcard address, and it holds the price
# book, the tenant table and the key issuer — so it refuses to start without
# this. That refusal is the point: a control plane listening on 0.0.0.0 with no
# token lets anything that can open a socket create a tenant, mint a working
# API key and rewrite what everyone is billed.
#
# It is written to a file rather than passed on the command line, because
# ExecStart= is world-readable through ps. The file is 0600, which is what the
# unit files themselves are not.
#
# Set ADMIN_TOKEN to choose it yourself; otherwise one is generated and kept, so
# a reinstall does not invalidate a token the console already has.
#
# A comma-separated list is accepted, so an operator can hand a second person
# their own credential. Revoking one is then an edit and a restart instead of a
# rotation that invalidates whatever the other is using.
if [ -z "${ADMIN_TOKEN:-}" ]; then
  if [ -s "$FLEET_HOME/state/admin.token" ]; then
    ADMIN_TOKEN=$(cat "$FLEET_HOME/state/admin.token")
  else
    ADMIN_TOKEN=$(head -c 32 /dev/urandom | base64 | tr -d '=+/' | cut -c1-40)
  fi
fi
mkdir -p "$FLEET_HOME/state"
umask 077
printf '%s' "$ADMIN_TOKEN" > "$FLEET_HOME/state/admin.token"
chmod 600 "$FLEET_HOME/state/admin.token"
umask 022
# Three names, one secret. Each process in this install is a separate module --
# the operator is a different repository -- so each keeps its own variable name
# rather than depending on the control plane's. The install script is the one
# place that knows they are the same value.
#
# The control plane gets the whole list; the gateway and the operator are handed
# only the first, because one shared credential is enough for a process that
# reads and reports rather than administers.
FIRST_TOKEN=${ADMIN_TOKEN%%,*}
cat > "$FLEET_HOME/state/admin.env" <<ENV
FLEET_ADMIN_TOKEN=$ADMIN_TOKEN
FLEET_CONTROL_PLANE_TOKEN=$FIRST_TOKEN
FLEET_REPORT_TOKEN=$FIRST_TOKEN
ENV
chmod 600 "$FLEET_HOME/state/admin.env"
if [ -n "$SKIP_OPERATOR" ]; then
cat > "$UNIT_DIR/fleet-upstream.service" <<EOF
[Unit]
Description=Resolve the Fleet gateway's upstream from k3s
Before=fleet-gateway.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=$FLEET_HOME/upstream.sh
EOF
fi

cat > "$UNIT_DIR/fleet-gateway.service" <<EOF
[Unit]
Description=Fleet gateway (OpenAI-compatible API and console)
After=fleet-apiserver.service
# Wants, not Requires: the gateway keeps serving the endpoints it already has
# when the control plane is unreachable. A gateway that dies with the control
# plane takes the traffic that is already in flight down with it, and the
# catalog client can tell "no deployments" from "could not ask" precisely so
# that this case does not have to be a restart.
Wants=fleet-apiserver.service

[Service]
ExecStart=$FLEET_HOME/bin/fleet-gateway
# Always, not on-failure: a server that exits cleanly without being asked has
# stopped serving, and treating exit 0 as success leaves the gateway down with
# no indication of why.
Restart=always
RestartSec=2
WorkingDirectory=$ROOT
Environment=FLEET_LISTEN=0.0.0.0:8080
EOF

if [ -n "${DATABASE_URL:-}" ]; then
cat >> "$UNIT_DIR/fleet-gateway.service" <<EOF
# The gateway needs the database as much as the control plane does: without it
# there are no tenants, no rate limits, no budgets and no ledger, so requests
# are served for free and nothing is ever billed. Routing and the console work
# either way, which is what makes the omission read as "billing is fine".
Environment=FLEET_DATABASE_URL=$DATABASE_URL
Environment=FLEET_DATABASE_MIGRATE=true
EOF
fi

if [ -z "$SKIP_OPERATOR" ]; then
cat >> "$UNIT_DIR/fleet-gateway.service" <<EOF
Environment=FLEET_CONTROL_PLANE_URL=$APISERVER
Environment=FLEET_CONTROL_PLANE_REFRESH=5s
# The same token, so endpoint discovery is not silently failing with a 401.
EnvironmentFile=$FLEET_HOME/state/admin.env
EOF
else
cat >> "$UNIT_DIR/fleet-gateway.service" <<EOF
Requires=fleet-upstream.service
EnvironmentFile=$FLEET_HOME/state/upstream.env
EOF
fi

cat >> "$UNIT_DIR/fleet-gateway.service" <<EOF

[Install]
WantedBy=multi-user.target
EOF

cat > "$UNIT_DIR/fleet-apiserver.service" <<EOF
[Unit]
Description=Fleet control plane
After=k3s.service

[Service]
# The token comes from a 0600 file rather than the command line: ExecStart= is
# visible to every local user through ps, and this credential can mint keys.
EnvironmentFile=$FLEET_HOME/state/admin.env
ExecStart=$FLEET_HOME/bin/fleet-apiserver --listen 0.0.0.0:8081 \\
  --allowed-origins http://$WSL_ADDR:8080 --data $FLEET_HOME/weights --hub stub${DATABASE_ARG}
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

if [ -z "$SKIP_OPERATOR" ]; then
cat > "$UNIT_DIR/fleet-operator.service" <<EOF
[Unit]
Description=Fleet operator (reconciles FleetModel and FleetDeployment)
After=k3s.service fleet-apiserver.service

[Service]
Environment=KUBECONFIG=/etc/rancher/k3s/k3s.yaml
# The same credential as the control plane, for the same reason it is in a 0600
# file rather than on the command line. Without it the inventory PUT is
# refused and the console's cluster page silently empties -- a page that looks
# broken rather than a 401 that names the cause.
EnvironmentFile=$FLEET_HOME/state/admin.env
ExecStart=$FLEET_HOME/bin/fleet-operator \\
  --metrics-bind-address :9090 --health-probe-bind-address :9091 --namespace $NS \\
  --report-to $APISERVER --report-every 5s --cluster-name $CLUSTER_NAME
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
fi

systemctl daemon-reload
systemctl enable fleet-apiserver fleet-gateway >/dev/null
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
  # Nothing resolves an address for the gateway now that it reads the control
  # plane, and the unit from an earlier install would sit there looking like it
  # still mattered. Remove it rather than leave it enabled and inert.
  systemctl stop fleet-upstream >/dev/null 2>&1 || true
  systemctl disable fleet-upstream >/dev/null 2>&1 || true
  rm -f "$UNIT_DIR/fleet-upstream.service"
  systemctl daemon-reload
else
  systemctl enable fleet-upstream >/dev/null
  systemctl restart fleet-upstream
fi
systemctl restart fleet-gateway

say "status"
for u in $UNITS; do
  printf '  %-18s %s\n' "$u" "$(systemctl is-active "$u" 2>/dev/null || true)"
done
printf '\n  console  http://%s:8080\n' "$WSL_ADDR"
printf '  control  http://%s:8081/api/v1\n' "$WSL_ADDR"
# Printed because the console needs it and there is nowhere else an operator
# would look: the console's Settings drawer asks for a "Control plane token",
# and this file is where it came from. Mode 600, so this is the one place it
# is disclosed.
printf '\n  console token  %s\n' "$FIRST_TOKEN"
printf '  (Settings -> Control plane token; stored in this browser only)\n'
printf '  to add another operator: ADMIN_TOKEN="%s,<new>" on the next install\n' "$FIRST_TOKEN"