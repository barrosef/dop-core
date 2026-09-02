export PATH := $(PATH):$(HOME)/go/bin

.PHONY: proto proto-breaking build test test-integration test-contract-integration lint run-serve run-worker migrate

proto:            ## generate Go from the .proto files (the source of truth — ADR-0017)
	cd api/proto && buf lint && buf generate

proto-breaking:   ## refuse an incompatible contract change
	cd api/proto && buf breaking --against '.git#subdir=api/proto'

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

migrate:          ## apply the migrations to the local Postgres
	@for f in migrations/*.sql; do \
	  echo "applying $$f"; \
	  kubectl cp $$f dop-local/postgres-0:/tmp/m.sql; \
	  kubectl exec -n dop-local postgres-0 -- sh -c \
	    "sed -n '/-- +goose Up/,/-- +goose Down/p' /tmp/m.sql | grep -v goose > /tmp/up.sql && psql -U dop -d dop -f /tmp/up.sql" ; \
	done
