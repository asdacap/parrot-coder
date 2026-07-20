# Testing

## Direct Go Gates

Go 1.25 or newer is required.

```sh
gofmt -w <changed-go-files>
go vet ./...
go test ./...
go test -race ./...
go mod verify
git diff --check
```

The built-binary smoke test is part of `go test ./...`. Manual equivalents are:

```sh
go build -o /tmp/parrot ./cmd/parrot
/tmp/parrot help
/tmp/parrot version
```

## Canonical Nix Gates

```sh
nix build
nix develop
nix flake check
nixfmt --check flake.nix
```

`nix flake check` runs the packaged build, Go formatting check, vet, and the
full test suite for the current flake system. CI runs it on Linux and separately
runs direct Go tests and the race detector on macOS and Linux.

Nix is not installed in every local development environment. If it is absent,
run all direct Go gates and rely on CI for the canonical flake evaluation; do
not report the Nix gate as locally passed.

## Race And Stress

```sh
go test -race ./...
go test -count=20 ./internal/event ./internal/session ./internal/agent \
  ./internal/change ./internal/process ./internal/httpapi \
  ./internal/transport/inproc ./internal/compaction ./internal/app \
  ./internal/cli ./internal/mcp ./internal/lsp ./internal/webfetch
```

Tests use local fixtures and bounded subprocesses. A failure under `-race` is a
release blocker even when the ordinary suite passes.

## Fuzzing

Each native target has deterministic seeds and performs no external network
access or process execution:

```sh
go test ./internal/change -run '^$' -fuzz '^FuzzParsePatch$' -fuzztime=10s
go test ./internal/protocol/sse -run '^$' -fuzz '^FuzzDecoder$' -fuzztime=10s
go test ./internal/config -run '^$' -fuzz '^FuzzParseYAML$' -fuzztime=10s
go test ./internal/mcp -run '^$' -fuzz '^FuzzFramedReader$' -fuzztime=10s
go test ./internal/lsp -run '^$' -fuzz '^FuzzReadFrame$' -fuzztime=10s
go test ./internal/terminal -run '^$' -fuzz '^FuzzSanitize$' -fuzztime=10s
```

Go writes newly discovered fuzz corpus entries below `testdata/fuzz`; review
them before adding them to source control.

## Cross Builds

Release targets are pure Go:

```sh
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
  GOOS=${target%/*} GOARCH=${target#*/} CGO_ENABLED=0 \
    go build -trimpath -o /tmp/parrot-${target%/*}-${target#*/} ./cmd/parrot
done
```
