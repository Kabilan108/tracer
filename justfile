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

sync *args:
    go run . sync {{args}}

watch *args:
    go run . watch {{args}}

nix-build:
    nix build .#tracer

nix-fmt:
    nixfmt flake.nix nix/hm-module.nix
