# faulttest

Automatically test how an HTTP service behaves when a request **succeeds on the
server but fails in transit** — the ambiguous completion that makes retries
unsafe.

The plan is to run a stateful application on loopback, forward traffic through a
proxy that destroys the client connection *after* the server has finished its
work, let the client retry, and then decide the verdict by reading the
application's actual state. A client that got a transport error has learned
nothing about whether its operation committed, so its HTTP status is not
evidence.

Full specification: [`docs/PRD.md`](docs/PRD.md).
Plan of work: [`docs/MILESTONES.md`](docs/MILESTONES.md).
Current state: [`docs/STATUS.md`](docs/STATUS.md).

## Status

Scaffold only. No fault injection, proxy, or fixture is implemented yet, and no
measured numbers appear anywhere in this repository.

## Requirements

Go 1.27.4 or newer (pinned in `go.mod`). No third-party dependencies.

## Build and verify

```console
$ go build ./cmd/faulttest
$ ./faulttest version
$ ./scripts/verify.sh        # gofmt, go vet, go build, go test
$ ./scripts/verify-full.sh   # the above plus go test -race
```

## Safety boundaries

This tool will deliberately break HTTP request semantics and will be able to
cause duplicate side effects. By design it only ever binds and connects to
loopback addresses, and there is no flag to target a remote service.
