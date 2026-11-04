.PHONY: all contract build test test-unit test-contract test-sandbox test-go test-sim sim local bootstrap attest serve status clean

all: build

## Build the token contract to contract/target/near/mergoshi_token.wasm (needs cargo-near).
## Without it, the commands fall back to the committed build in contract/res/.
contract:
	cd contract && cargo near build non-reproducible-wasm --no-embed-abi
	@if command -v wasm-opt >/dev/null; then \
		wasm-opt -Oz --enable-bulk-memory --enable-sign-ext --enable-mutable-globals --strip-debug --strip-producers \
			contract/target/near/mergoshi_token.wasm -o contract/target/near/mergoshi_token.wasm; \
	fi
	@ls -l contract/target/near/mergoshi_token.wasm

## Build the Go binary to bin/mergoshi.
build:
	cd services && go build -o ../bin/mergoshi ./cmd/mergoshi

## Every test: contract unit tests (incl. the golden replay), sandbox integration, Go, Python.
test: test-contract test-sandbox test-go test-sim

## Tests that need no network.
test-unit: test-contract test-go test-sim

test-contract:
	cd contract && cargo test --lib

test-sandbox:
	cd contract && cargo test --test sandbox

test-go:
	cd services && go vet ./... && go test ./...

test-sim:
	python3 sim/golden.py --check
	python3 -m unittest discover -s sim/tests -t sim

## Rerun the simulation and redraw the charts in sim/out.
sim:
	python3 sim/run.py

## Everything on this machine: sandbox node, token, attesters, dashboard, fake GitHub.
local: build
	./bin/mergoshi local -reset

## Testnet: create accounts through the public helper, deploy, launch, bond (idempotent).
bootstrap: build
	./bin/mergoshi bootstrap

attest: build
	./bin/mergoshi attest

serve: build
	./bin/mergoshi serve -attesters

status: build
	./bin/mergoshi status

clean:
	rm -rf bin contract/target .localnet/home .localnet/env .localnet/state
