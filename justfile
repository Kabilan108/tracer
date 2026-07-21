set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

default: test

build:
    go build -ldflags "-X main.version=dev-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" -o ./bin/tracer .

clean:
    rm -rf ./bin/*

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

# Pre-flight for cutting a release: run before bumping the flake version.
release-check:
    test -z "$(gofmt -l . | grep -v '^[.]tracer' || true)" || { gofmt -l . | grep -v '^[.]tracer'; exit 1; }
    go vet ./...
    go test ./...
    @echo "flake version: $(nix eval --raw .#tracer.version)"
    nix build .#tracer
    test "$(NO_COLOR=1 ./result/bin/tracer version)" = "tracer $(nix eval --raw .#tracer.version)"
    @echo "release-check passed"

nix-fmt:
    nixfmt flake.nix nix/hm-module.nix
