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
arrives from the operator running inside the cluster, which reports it to the
control plane (`--report-to=http://127.0.0.1:8081`); the gateway then learns
its endpoints from that same inventory (`FLEET_CONTROL_PLANE_URL`), so
scaling a deployment reaches the router without restarting anything. A cluster
page that is empty on a fresh checkout is the correct state rather than a
missing feature — nothing has reported yet.

## Try it

One command. It builds the console if it is not built, starts the gateway and
the control plane, and needs no GPU, no S3, and no network:

```sh
./scripts/dev.sh
```

Then open <http://127.0.0.1:8080>. Four pages: a chat playground, live fleet
state, the model registry, and cluster inventory. To fill them with data:

```sh
./scripts/seed.sh
```

`dev.sh --real` does the same but pulls from huggingface.co instead of a
synthetic repository. Everything lands in `.dev/`, which is gitignored.

### Proving it works

`seed.sh` fills the pages; it does not check anything. To check:

```sh
./scripts/smoke.sh        # in a second terminal, with dev.sh running
```

62 assertions, and it exits non-zero on the first failure. It is safe to re-run.
What it covers, and why each check exists:

| Area | Checks |
|---|---|
| OpenAI compatibility | a real completion, a 54-frame stream, a `usage` object on both, `[DONE]`, error envelopes instead of a router's 404 page |
| Console | the app shell, a deep link, and that each `/assets/*.js` is served as JavaScript **and is not the app shell** — a 200 that is really `index.html` is the failure that hides |
| CORS | a loopback origin is allowed and a foreign one is not, because the console is on :8080 and the control plane answers on :8081 |
| Engine profiles | vLLM demands compute 7.5+ and llama.cpp demands none; vLLM maps cache occupancy to a real series and publishes KV capacity, llama-cpp publishes queue signals but no occupancy signal and no capacity |
| Weight formats | a safetensors and a GGUF repository side by side, each passing its own engine and each refused by the other's, with the reason in the message |
| Operator inventory | counts recomputed from the node list, and `Scheduling` with a reason kept distinct from `Pending` |
| Authentication | health without a key, a valid key served, a missing and an unknown key both 401, and the 401 advertising `WWW-Authenticate` |
| Rate limiting | a per-minute request ceiling, a token ceiling that bites before the request ceiling, `Retry-After` on the refusal, one tenant's keys sharing a bucket, and another tenant staying unaffected |

The last two rows run against a **second** gateway the script starts itself, on
port 8099 and 8098. The main gateway on :8080 has authentication off — a
developer's laptop must work with no setup — so it cannot check this surface,
and leaving it unchecked is how a gateway ships that serves anyone's requests.

That last row is the one to read twice. A `Ready` model that cannot be loaded
is a claim the platform cannot back up, and the pull is where that claim is
made.

`make check` runs what CI runs and needs no server. `smoke.sh` is separate
because it needs both processes up, so it cannot be part of `check`.

### Turning authentication on

Off by default, because a developer should be able to run the console with no
setup. On in production, and the gateway refuses to start rather than serving
everyone when the two halves of the configuration disagree:

```sh
export FLEET_AUTH_REQUIRED=true
export FLEET_API_KEYS='acme/research/team-a;acme/batch/worker'
export FLEET_RATE_TENANTS='acme|rpm=1200,tpm=400000'
export FLEET_RATE_PROJECTS='acme/research|rpm=600,tpm=200000;acme/batch|rpm=60,tpm=20000'
export FLEET_DEFAULT_MAX_TOKENS=4096 # what an unbounded request reserves against
```

A key is `tenant/project/keyid`, and that whole string is also the credential
the client sends as its bearer token. Keys carry **no limits** — a key rotates
and leaks, so a budget attached to one would have to be reissued along with it,
and lowering a budget would invalidate whatever the caller was using when they
were cut off.

Limits live on two levels, and **both are checked on every request**:

| | Declared in | Bounds |
|---|---|---|
| Envelope | `FLEET_RATE_TENANTS` | everything the tenant spends, across all its projects |
| Partition | `FLEET_RATE_PROJECTS` | one project, within its tenant's envelope |

They are two counters rather than one merged figure because a project limit on
its own is arithmetic the tenant performs: ten projects at 600 rpm is 6000, and
no per-project number would ever have bound. The envelope is what makes the
partitions add up. A project limit above its tenant's envelope is refused at
startup rather than clamped — it could never apply, and an operator who wrote one
believes they have divided a budget they have not divided.

`FLEET_RATE_RPM` and `FLEET_RATE_TPM` are the server defaults for a tenant
nobody declared. A limit that says only `rpm` stays unlimited on the token
dimension — for a *partition* that means "no limit of its own here", which is
not the same as inheriting the tenant default.

Policies are read once per scope and cached, so changing a limit needs a gateway
restart. Three configurations are refused at startup — `required` with no keys,
keys without `required`, and a key spec that does not parse — because each of
them looks like a working gateway from the outside while serving nobody. A key
spec still carrying `|rpm=…` is also refused, with a message saying where limits
moved, rather than being silently accepted with the numbers dropped.

`/healthz` and `/health` stay unauthenticated, because a Kubernetes probe
cannot hold a credential and a 401 there reports a healthy pod as dead.

### What each page does in the default run

| Page | Backed by | Needs |
|---|---|---|
| Playground | an in-process stub engine (`--demo`) | nothing |
| Fleet | the gateway's own request log | nothing |
| Models | `fleet-apiserver` + a directory as the object store | `dev.sh` |
| Cluster | an operator inventory report | `seed.sh`, or a real operator |

The stub engine's replies are canned. The point is to exercise streaming,
usage accounting and routing, all of which behave the same against a real
engine. The stub honours `stream_options.include_usage` exactly as vLLM does,
including *omitting* the final usage frame when the client did not ask for
one, so the metering path is genuinely covered.

The synthetic hub serves a few kilobytes of plausible filenames for three
repositories — including one that fails mid-pull and one slow enough to cancel.
That is enough to exercise the whole pull path, progress reporting and
cancellation, without anyone downloading 15 GiB to test a progress bar.

### Against a real engine

```sh
fleet-gateway --config fleet.yaml
```

```yaml
listen: ":8080"
upstreams:
  - id: llama-7b
    model: Qwen/Qwen2.5-7B-Instruct
    base_url: http://127.0.0.1:8001   # llama-server, or a vLLM Service
    replicas: 1
    api_key: ""                       # the engine's key, not a tenant's
```

### Against MinIO or S3

The control plane uses a directory when `FLEET_S3_ENDPOINT` is unset, and any
S3-compatible endpoint when it is set:

```sh
export FLEET_S3_ENDPOINT=127.0.0.1:9000
export FLEET_S3_BUCKET=fleet
export FLEET_S3_ACCESS_KEY=minioadmin
export FLEET_S3_SECRET_KEY=minioadmin
export FLEET_S3_USE_SSL=false
fleet-apiserver --listen :8081
```

The `FLEET_S3_` prefix deliberately shadows the `AWS_` names: a process that
also talks to AWS must not pick up Fleet's bucket by accident.

## Development

```sh
make check          # gofmt + go vet + no-cgo check + tests + console types
make smoke          # end-to-end assertions; needs ./scripts/dev.sh running
make build          # host binaries into ./bin
make web            # build the console and stage it for embedding
make dev            # the same as ./scripts/dev.sh
make seed           # fill the dev control plane with data
make web-dev        # console dev server on :5173, proxying to a local gateway
make build-release  # linux/amd64, linux/arm64, darwin/arm64
make help           # all targets
```

Requires Go 1.26+ and Node 20+.

`scripts/dev.sh`, `scripts/seed.sh` and `scripts/smoke.sh` do not need make —
the Makefile targets are aliases so the sequences are discoverable.

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
