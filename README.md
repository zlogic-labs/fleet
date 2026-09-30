# Fleet

An open-source platform for self-hosting open models: an OpenAI-compatible
gateway with metering, and Kubernetes-native model deployment on GPU.

Repository: <https://github.com/zlogic-labs/fleet>

**Status: early development.** The design is settled and documented; the
implementation is being built module by module against it.

## Why

Self-hosting an open model is not a GPU problem, it is an operations problem.
The GPU is the easy half. What teams actually lack is a way to give the model
an OpenAI-compatible endpoint, decide who may call it, charge them for what
they used, and place it on hardware that is not fully utilised today.

Fleet targets that gap, and it targets the part of it nobody has built: the
economics of a fleet whose cost is **fixed** rather than metered per token.
See [docs/architecture.md](docs/architecture.md) §6.

## Design commitments

These are the rules the code is written against. Changing one requires an ADR.

| | |
|---|---|
| **P1** | The OpenAI protocol is the only contract with an inference engine. No vendor SDK is ever imported. |
| **P2** | Scale replicas horizontally, not cards vertically. |
| **P3** | The gateway owns endpoint selection, because it is the only layer holding token counts and quota state. |
| **P4** | Fleet owns its CRDs. Upstream operators are rendered to, never adopted as the API. |
| **P5** | Rate limits are reserved up front and settled after, never deducted after the fact. |
| **P6** | Only the engine's `usage` field is trusted for billing. |
| **P7** | The gateway does not depend on Kubernetes. Enforced by the module graph, not by review. |
| **P8** | Billing allocates a fixed cost pool by GPU-hour; it is not a prepaid balance. |

## Layout

Two Go modules, split so that P7 is a compile error rather than a convention.

```
core/      gateway, control plane, billing, storage        no k8s dependency
operator/  CRDs, reconcilers, GPU scheduling               depends on core
web/       operator console (React + antd)                 builds into core
docs/      architecture and ADRs
```

Neither `core` nor the console reads Kubernetes. Node and GPU inventory
arrives from the operator running inside the cluster and is reported to the
control plane, which is why the cluster page can be empty on a fresh checkout
and that is the correct state rather than a missing feature.

## Try it

```sh
make web                        # build the console once
go run ./core/cmd/fleet-gateway --demo
```

Then open <http://127.0.0.1:8080>. Four pages: a chat playground, live fleet
state, the model registry, and cluster inventory.

`--demo` starts a built-in stub engine in the same process, so nothing needs
to be installed and no GPU is involved. The replies are canned — the point is
to exercise streaming, usage accounting and routing, all of which behave the
same against a real engine. The stub honours `stream_options.include_usage`
exactly as vLLM does, including *omitting* the final usage frame when the
client did not ask for one, so the metering path is genuinely covered.

To point at a real engine instead:

```sh
fleet-gateway --config fleet.yaml
```

```yaml
listen: ":8080"
upstreams:
  - id: llama-7b
    model: Qwen/Qwen2.5-7B-Instruct
    base_url: http://127.0.0.1:8081   # llama-server, or a vLLM Service
    replicas: 1
    api_key: ""                       # the engine's key, not a tenant's
```

## Development

```sh
make check          # gofmt + go vet + no-cgo check + tests + console types
make build          # host binaries into ./bin
make web            # build the console and stage it for embedding
make web-dev        # console dev server on :5173, proxying to a local gateway
make build-release  # linux/amd64, linux/arm64, darwin/arm64
make help           # all targets
```

Requires Go 1.26+ and Node 20+.

The console bundle is built by `web/` and embedded into the gateway binary; it
is not committed. A fresh clone still compiles — the binary serves a
placeholder telling you to run `make web` — and `build-release` depends on it
so a release never ships a binary with the placeholder in it.

Fleet is pure Go. There is no cgo anywhere in the dependency graph and no
architecture-specific code, so every release binary is statically linked and
cross-compiles from any host. `make check-cgo` and the CI build matrix both
enforce this — a dependency that only builds on one architecture fails CI
rather than a customer's cluster.

| Target | Why |
|---|---|
| `linux/amd64` | NVIDIA, and the mainstream domestic GPU cards |
| `linux/arm64` | Ascend 910B on Kunpeng, Apple Silicon hosts |
| `darwin/arm64` | local development |

> If your `go` reports `go1.x windows/386`, the toolchain is 32-bit. That is a
> workstation problem, not a Fleet one — install a 64-bit Go. `go test -race`
> additionally needs a C toolchain, because the race detector is cgo-based.

## Local cluster

k3s under WSL2, for control-plane work only — see
[docs/architecture.md](docs/architecture.md) §9 for the WSL2-specific
limitations (no GPU Operator, no MIG). GPU-specific behaviour is verified in CI
on bare Linux.

## License

Apache 2.0 — see [LICENSE](LICENSE). Third-party dependency licenses are
recorded in [NOTICE](NOTICE).

Copyright 2026 Zlogic Labs.

## Contributing

Not open for outside contributions at this stage. Issues and PRs from early
adopters are welcome once the first release is cut.
