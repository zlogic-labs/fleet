# Local development cluster

k3s under WSL2, for control-plane work. Bring it up with:

```sh
sudo -E bash deploy/k3s-dev/setup.sh
```

From Windows, `wsl -d Ubuntu -e kubectl get pods -A` works because WSL forwards
localhost; the API server is on `https://127.0.0.1:6443` and the kubeconfig is
`/etc/rancher/k3s/k3s.yaml`.

## What this cluster verifies

Everything that is Fleet's own logic and needs no GPU:

- CRD lifecycle and reconciliation, rollout and status conditions
- Autoscaler decisions, given metrics fed from a fake source
- The gateway end to end: routing, rate limiting, quota reserve and settle,
  billing and the usage ledger
- SSE passthrough and usage collection, against any OpenAI-compatible fake

## What it cannot verify

The GPU half, because WSL2 changes the substrate:

| | Why |
|---|---|
| **MIG** | Not exposed to WSL2 guests at all. MIG partitioning cannot be tested here under any configuration. |
| **GPU Operator** | Installs kernel modules (`nvidia.ko`, the container toolkit shim). WSL2 replaces the kernel with a paravirtualised driver, so the operator cannot work. Use the device plugin directly, as `setup.sh` does. |
| **NVML enumeration** | Frequently returns zero devices from inside a container even when `nvidia-smi` works on the host. The node then registers the plugin but advertises no `nvidia.com/gpu`. |
| **Cilium** | Depends on eBPF features the WSL2 kernel does not fully expose. k3s defaults to flannel; leave it. |
| **Model weights** | Must live inside the WSL filesystem. Anything on `/mnt/c` reaches the model through the 9p protocol and safetensors load an order of magnitude slower. |

Topology-aware placement and multi-node tensor/pipeline parallelism need real
hardware with RDMA. Those belong in CI on bare Linux.

## Environment notes

- WSL2 only. WSL1 has no kernel for k3s to use.
- systemd must be PID 1: add `[boot]` then `systemd=true` to `/etc/wsl.conf`.
- `swapoff -a` runs at setup, but WSL re-creates swap on the next boot unless
  it is disabled in `%UserProfile%\.wslconfig`.
- `unattended-upgrade` is stopped and its timers disabled by the setup script.
  On a slow or proxied link it holds the dpkg lock for many minutes and makes
  any `apt-get install` fail with a bare lock error.

Go lives at `/usr/local/go` and is on the PATH via `/etc/profile.d/golang.sh`
and `~/.bashrc`. Both exist because systemd units and k3s hooks do not read
`~/.bashrc`.

Building the module from `/mnt/d` works — the 9p source is slower than ext4,
but the Go build cache is on ext4 where the real time goes, so a cold build of
the current tree is around 20 seconds.
