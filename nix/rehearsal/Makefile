# This Makefile exists so the repository-wide sweeps (`make test` / `make lint`,
# which run `make -C <module> <target>` for every module in go.work) work with
# the rehearsal driver module in the workspace. The harness itself is driven by
# nix/rehearsal/run-matrix.sh and `make rehearsal` from the repository root.

.PHONY: test fmt

# The driver's tests are pure unit tests: no object store, no VM, no credentials.
test:
	go test -count=1 ./...

fmt:
	gofmt -l .
