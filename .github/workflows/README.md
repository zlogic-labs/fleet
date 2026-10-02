# Continuous integration is switched off

`ci.yml` was removed on 2026-10-02 and is not reinstated until the project's
free build minutes are no longer the binding constraint.

It is removed rather than disabled so that nothing runs at all — a disabled
workflow still shows up in the Actions tab, still gets edited, and still costs
review time to keep in step with a Makefile it is not running.

The checks it performed are all still available locally and are the same ones
`make check` runs, so nothing is lost but the automatic trigger:

| Was | Now |
|---|---|
| `go build` on four platforms | `make build-release` |
| no-cgo assertion | `make check-cgo` |
| `go vet` | `make vet` |
| `go test -race -coverprofile` | `make test-race` |
| `gofmt` | `make fmt-check` |
| web typecheck + build | `make web-typecheck` + `make web` |

The one thing genuinely lost is running them against every push from a Linux
runner, which is why `test-race` needs a C toolchain locally: the Windows Go
toolchain here is 386-bit and `-race` requires cgo.
