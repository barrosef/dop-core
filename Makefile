export PATH := $(PATH):$(HOME)/go/bin

# CONTEXT guards `migrate` (see the `guard` target below). It names the ONLY
# kube-context `make migrate` is allowed to run against — kept in step with
# dop-infra/Makefile's own CONTEXT by hand, since the two repositories are not
# wired to share one value.
CONTEXT := k3d-dop-local

.PHONY: proto proto-breaking build test test-integration test-contract-integration lint run-serve run-worker guard migrate seed

proto:            ## generate Go from the .proto files (the source of truth — ADR-0013)
	cd api/proto && buf lint && buf generate

proto-breaking:   ## refuse an incompatible contract change
	@# Run from the REPOSITORY ROOT, not from api/proto. `.git#subdir=api/proto`
	@# is resolved relative to the working directory, so `cd api/proto` first made
	@# buf look for api/proto/api/proto — the gate reported nothing and looked
	@# green. It ran that way for months.
	@#
	@# And `branch=main` is not decoration either: without it buf compares the
	@# working tree against the current HEAD, so the moment a change is committed
	@# the gate compares that commit with itself and passes by construction. A
	@# gate that cannot fail after you commit is a gate that never guarded a
	@# merge.
	buf breaking api/proto --against '.git#branch=main,subdir=api/proto'

build:
	go build -o bin/dop-core ./cmd/dop-core

test:             ## unit + contract + architecture (needs no environment)
	go test ./... -count=1

test-integration: ## the event spine against the local environment
	@echo "requires: kubectl port-forward svc/postgres 5432 and svc/nats 4222"
	go test ./test/integration/ -tags=integration -v -count=1

test-contract-integration: ## contract suites against the REAL adapters
	@echo "requires: kubectl port-forward svc/nats 4222, svc/firebase 9199 and"
	@echo "         svc/secretmanager 8085:9090 (the Secret Manager emulator gRPC)"
	@echo "excluded: 8_metadata (the emulator hangs on application/json) and"
	@echo "13_concurrent_use (~16 simultaneous operations BRING THE EMULATOR DOWN)."
	@echo "Emulator defects, not adapter ones — the fs adapter passes all 13."
	go test ./test/contract/ -tags=integration -v -count=1 \
	  -skip 'TestObjectStoreContractGCS/gcs-emulated/(8_metadata|13_concurrent_use)' 

lint:
	go vet ./...
	@gofmt -l . | tee /dev/stderr | (! read)

run-serve: build
	./bin/dop-core serve

run-worker: build
	./bin/dop-core worker

# guard, copied from dop-infra/Makefile's own: a machine that holds clients'
# production kube contexts cannot run an unguarded `kubectl exec`. `migrate`
# used to run `kubectl cp` and `kubectl exec` against WHATEVER context happened
# to be active — fine on a laptop with only the local cluster configured, and a
# live incident the day this same machine also holds a client's production
# context and somebody runs `make migrate` without checking `kubectl config
# current-context` first. The guard makes that check unskippable.
guard:
	@ctx=$$(kubectl config current-context 2>/dev/null); \
	if [ "$$ctx" != "$(CONTEXT)" ]; then \
		echo "ABORTED — the active context is '$$ctx', expected '$(CONTEXT)'."; \
		echo "Use: kubectl config use-context $(CONTEXT)"; \
		exit 1; \
	fi

migrate: guard    ## apply the embedded migrations to the local Postgres (ADR-0024)
	@# The binary applies its own migrations and records them in
	@# goose_db_version; the worker does the same at boot. This target exists
	@# for the developer who changed a migration and wants it applied NOW,
	@# without restarting the worker. `make migrate ARGS=status` lists;
	@# `ARGS="baseline 27"` is the one-time bootstrap of a hand-migrated database.
	@kubectl -n dop-local port-forward svc/postgres 15432:5432 >/dev/null 2>&1 & \
	  pf=$$!; sleep 2; \
	  DATABASE_URL="postgres://dop:dop-local-dev@127.0.0.1:15432/dop?sslmode=disable" \
	    go run ./cmd/dop-core migrate $(ARGS); rc=$$?; kill $$pf; exit $$rc

seed: guard       ## apply the embedded seeds (root + DOP_SEED_PROFILE=local) to the local Postgres
	@kubectl -n dop-local port-forward svc/postgres 15432:5432 >/dev/null 2>&1 & \
	  pf=$$!; sleep 2; \
	  DATABASE_URL="postgres://dop:dop-local-dev@127.0.0.1:15432/dop?sslmode=disable" \
	  DOP_SEED_PROFILE=local go run ./cmd/dop-core seed; rc=$$?; kill $$pf; exit $$rc
