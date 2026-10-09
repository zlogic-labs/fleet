#!/usr/bin/env bash
# Route the Kubernetes pod and service networks around the host's proxy TUN.
#
# mihomo's auto-route rules send every DNS packet on the host into its TUN
# device — including the pod network's queries to CoreDNS. A pod then resolves
# fleet-postgres.fleet.svc.cluster.local to a fake address in 198.18.0.0/16,
# the connection walks out to the physical gateway, and every name lookup in
# the cluster times out. It presents as a database problem.
#
# One policy-routing rule at a priority mihomo does not own sends those two
# networks to the main table first, where the real routes live. It is additive
# on purpose: mihomo's config is untouched and keeps handling everything it is
# there for. The rule is harmless on a host with no proxy at all, which is why
# install.sh runs it unconditionally.
set -euo pipefail

POD_NET=${POD_NET:-10.42.0.0/16}
SVC_NET=${SVC_NET:-10.43.0.0/16}
PRIORITY=${PRIORITY:-8999}
SUDO_PASSWORD=${SUDO_PASSWORD:-root}

say() { printf '\n== %s\n' "$1"; }

sudo_n() {
  if [ "$(id -u)" = 0 ]; then
    "$@"
    return
  fi
  if sudo -n true 2>/dev/null; then
    sudo "$@"
    return
  fi
  printf '%s\n' "$SUDO_PASSWORD" | sudo -S -p '' "$@"
}

say "routing $POD_NET and $SVC_NET to the main table (priority $PRIORITY)"
for net in "$POD_NET" "$SVC_NET"; do
  if ip rule show | grep -q "to $net lookup main"; then
    printf '  %s already routed\n' "$net"
  else
    sudo_n ip rule add priority "$PRIORITY" to "$net" lookup main
    printf '  %s -> main\n' "$net"
  fi
done

say "installing the unit that keeps the rules across a reboot"
sudo_n tee /etc/systemd/system/fleet-cluster-routes.service >/dev/null <<EOF
[Unit]
Description=Route the Kubernetes pod and service networks around the proxy TUN
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'ip rule show | grep -q "to $POD_NET lookup main" || ip rule add priority $PRIORITY to $POD_NET lookup main; ip rule show | grep -q "to $SVC_NET lookup main" || ip rule add priority $PRIORITY to $SVC_NET lookup main'
ExecStop=/bin/sh -c 'ip rule del priority $PRIORITY to $POD_NET lookup main || true; ip rule del priority $PRIORITY to $SVC_NET lookup main || true'

[Install]
WantedBy=multi-user.target
EOF
sudo_n systemctl daemon-reload
sudo_n systemctl enable --now fleet-cluster-routes.service

say "rules now in force"
ip rule show | grep -E "($POD_NET|$SVC_NET)" || echo "  (none — the fix did not apply)"
