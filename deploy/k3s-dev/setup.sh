#!/usr/bin/env bash
# Bring up the local development cluster: k3s plus an NVIDIA device plugin.
#
# This targets WSL2 on Ubuntu. See README.md in this directory for what the
# resulting cluster can and cannot verify.
#
# Idempotent: re-running repairs a partial install rather than failing.

set -euo pipefail

DEVICE_PLUGIN_VERSION="${DEVICE_PLUGIN_VERSION:-v0.17.0}"
K3S_CHANNEL="${K3S_CHANNEL:-stable}"
K3S_CONFIG=/etc/rancher/k3s/k3s.yaml
CONTAINERD_CONF=/etc/containerd/conf.d

log() { printf '\n=== %s ===\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

require_root() { [ "$(id -u)" -eq 0 ] || die "run as: sudo -E bash $0"; }

require_wsl2() {
  grep -q microsoft-standard-WSL2 /proc/sys/kernel/osrelease 2>/dev/null \
    || die "this cluster is WSL2-only; WSL1 has no real kernel for k3s"
}

require_systemd() {
  [ "$(ps -p 1 -o comm=)" = "systemd" ] \
    || die "systemd is not PID 1; add [boot] systemd=true to /etc/wsl.conf and restart WSL"
}

disable_gpu() {
  # Swap confuses the kubelet. WSL may re-create it on the next boot, so this
  # is best-effort and the script does not fail when it cannot.
  log "disabling swap"
  swapoff -a 2>/dev/null || log "swap still active; kubelet may warn about it"
}

install_k3s() {
  if command -v k3s >/dev/null 2>&1; then
    log "k3s already installed: $(k3s --version | head -1)"
    return
  fi
  log "installing k3s from the ${K3S_CHANNEL} channel"
  local ip
  ip="$(ip -4 addr show eth0 | grep -oP 'inet \K[\d.]+' | head -1)"
  curl -sfL https://get.k3s.io \
    | INSTALL_K3S_CHANNEL="${K3S_CHANNEL}" \
      INSTALL_K3S_EXEC="--write-kubeconfig-mode 644 --tls-san ${ip}" \
      sh -
}

install_nvidia_toolkit() {
  if command -v nvidia-ctk >/dev/null 2>&1; then
    log "nvidia-container-toolkit already installed: $(nvidia-ctk --version | head -1)"
    return
  fi
  # WSL2 ships the driver libraries but does not put them on the loader path.
  log "exposing WSL driver libraries to ldconfig"
  echo /usr/lib/wsl/lib > /etc/ld.so.conf.d/wsl-nvidia.conf
  ldconfig

  log "adding the NVIDIA container toolkit repository"
  rm -f /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -fsSL --retry 8 --retry-all-errors --retry-delay 2 -m 90 \
    https://nvidia.github.io/libnvidia-container/gpgkey \
    | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  # The downloaded key must not be zero bytes, or every later apt call fails
  # with an unhelpful signature error.
  [ "$(stat -c%s /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg)" -gt 100 ] \
    || die "GPG key came back empty; the download did not succeed"

  curl -sSL --retry 8 --retry-all-errors --retry-delay 2 -m 90 \
    https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
    > /etc/apt/sources.list.d/nvidia-container-toolkit.list

  log "installing nvidia-container-toolkit"
  # unattended-upgrades holds the dpkg lock for minutes on a slow link and
  # makes this step fail with a bare "could not get lock" error.
  systemctl stop unattended-upgrades 2>/dev/null || true
  systemctl disable --now apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
  DEBIAN_FRONTEND=noninteractive apt-get update -qq -o Acquire::Retries=5
  DEBIAN_FRONTEND=noninteractive dpkg --configure -a || true
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nvidia-container-toolkit
}

configure_containerd() {
  log "registering the nvidia runtime with containerd"
  # With containerd 2.x, nvidia-ctk writes a fragment into conf.d rather than
  # editing the target file. k3s imports /etc/containerd/conf.d/*.toml, so the
  # fragment is picked up and runc stays the default runtime.
  mkdir -p "${CONTAINERD_CONF}"
  nvidia-ctk runtime configure --runtime=containerd

  if ! kubectl get runtimeclass nvidia >/dev/null 2>&1; then
    log "creating the nvidia RuntimeClass"
    kubectl create -f - <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: nvidia
handler: nvidia
YAML
  fi

  log "restarting k3s so containerd reloads its config"
  systemctl restart k3s
  wait_for_node
}

install_device_plugin() {
  log "applying the NVIDIA device plugin (${DEVICE_PLUGIN_VERSION})"
  kubectl apply -f \
    "https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/${DEVICE_PLUGIN_VERSION}/deployments/static/nvidia-device-plugin.yml"

  # The upstream manifest hardcodes namespace: kube-system and leaves the
  # plugin's own Pod on the default runtime. Without the runtime class the Pod
  # has no NVML, so the plugin registers its socket while reporting zero
  # devices and the node never advertises nvidia.com/gpu.
  log "pinning the plugin Pod to the nvidia runtime"
  kubectl -n kube-system patch daemonset nvidia-device-plugin-daemonset --type=merge \
    -p '{"spec":{"template":{"spec":{"runtimeClassName":"nvidia"}}}}'
  kubectl -n kube-system rollout status daemonset/nvidia-device-plugin-daemonset --timeout=180s
}

wait_for_node() {
  export KUBECONFIG="${K3S_CONFIG}"
  log "waiting for the node to become Ready"
  for _ in $(seq 1 60); do
    if kubectl get nodes --no-headers 2>/dev/null | grep -q Ready; then
      return
    fi
    sleep 5
  done
  die "node did not become Ready"
}

report() {
  export KUBECONFIG="${K3S_CONFIG}"
  log "nodes"
  kubectl get nodes -o wide
  log "pods"
  kubectl get pods -A
  log "GPU resources"
  # The plugin registers its socket even when NVML enumerates nothing, so the
  # resource appears with a count of 0. Only a positive count means a GPU is
  # actually schedulable.
  local gpu
  gpu="$(kubectl get nodes -o jsonpath='{.items[*].status.allocatable.nvidia\.com/gpu}' 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+$' | head -1)"
  if [ -n "${gpu}" ] && [ "${gpu}" -gt 0 ]; then
    kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu
  else
    cat <<'EOF'
  nvidia.com/gpu is not schedulable on this node.

  The device plugin is running and has registered its socket, but it reports
  zero devices. Check, in order:
    1. nvidia-smi runs and reports a card
    2. nvidia-container-cli info lists a device
    3. kubectl -n kube-system logs ds/nvidia-device-plugin-daemonset

  Under WSL2 NVML frequently cannot enumerate the host GPU from inside a
  container. That is an environment limitation, not a Fleet one — verify GPU
  behaviour on real hardware. See deploy/k3s-dev/README.md.
EOF
  fi
}

require_root
require_wsl2
require_systemd

export KUBECONFIG="${K3S_CONFIG}"
disable_gpu
install_k3s
install_nvidia_toolkit
wait_for_node
configure_containerd
install_device_plugin
report

log "done"
cat <<EOF
  kubectl:  KUBECONFIG=${K3S_CONFIG} kubectl get pods -A
  from Windows (WSL forwards localhost):
    wsl -d Ubuntu -e kubectl get pods -A
    https://127.0.0.1:6443  (kubeconfig at ${K3S_CONFIG})
EOF
