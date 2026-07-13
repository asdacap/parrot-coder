# Parrot Coder

Parrot Coder is a local-first coding agent written in Go. It keeps the agent
runtime reviewable, uses a normal append-only terminal interface, and supports
ChatGPT subscription login and OpenAI-compatible endpoints.

The project is under active development. The architectural contracts are in
[`docs/`](docs/).

## Development

The canonical development environment is the Nix flake:

```sh
nix develop
```

Run the local quality gates:

```sh
nix flake check
```

The equivalent direct Go checks require Go 1.25 or newer:

```sh
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
```

Build the binary:

```sh
nix build
# or, inside `nix develop`:
go build -o bin/parrot ./cmd/parrot
```
