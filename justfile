set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

default: test

build:
    go build -o ./bin/tracer .

test:
    go test ./...

fmt:
    gofmt -w .

run *args:
    go run . {{args}}

ingest *args:
    go run . ingest {{args}}

daemon *args:
    go run . daemon run {{args}}

nix-build:
    nix build .#tracer

nix-fmt:
    nixfmt flake.nix nix/hm-module.nix
