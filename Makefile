export PATH := $(PATH):$(HOME)/go/bin

.PHONY: proto proto-breaking build test test-integration test-contract-integration lint run-serve run-worker migrate

proto:            ## gera Go a partir dos .proto (fonte da verdade — ADR-0017)
	cd api/proto && buf lint && buf generate

proto-breaking:   ## recusa mudança incompatível de contrato
	cd api/proto && buf breaking --against '.git#subdir=api/proto'

build:
	go build -o bin/dop-core ./cmd/dop-core

test:             ## unidade + contrato + arquitetura (não exige ambiente)
	go test ./... -count=1

test-integration: ## espinha de eventos contra o ambiente local
	@echo "exige: kubectl port-forward svc/postgres 5432 e svc/nats 4222"
	go test ./test/integration/ -tags=integration -v -count=1

test-contract-integration: ## suítes de contrato contra os adaptadores REAIS
	@echo "exige: kubectl port-forward svc/nats 4222 e svc/firebase 9199"
	@echo "excluídos 8_metadados (o emulador pendura com application/json) e"
	@echo "13_uso_concorrente (~16 operações simultâneas DERRUBAM o emulador)."
	@echo "Defeitos do emulador, não do adaptador — o fs passa nos 13."
	go test ./test/contract/ -tags=integration -v -count=1 \
	  -skip 'TestObjectStoreContractGCS/gcs-emulado/(8_metadados|13_uso_concorrente)' 

lint:
	go vet ./...
	@gofmt -l . | tee /dev/stderr | (! read)

run-serve: build
	./bin/dop-core serve

run-worker: build
	./bin/dop-core worker

migrate:          ## aplica as migrações no Postgres local
	@for f in migrations/*.sql; do \
	  echo "aplicando $$f"; \
	  kubectl cp $$f dop-local/postgres-0:/tmp/m.sql; \
	  kubectl exec -n dop-local postgres-0 -- sh -c \
	    "sed -n '/-- +goose Up/,/-- +goose Down/p' /tmp/m.sql | grep -v goose > /tmp/up.sql && psql -U dop -d dop -f /tmp/up.sql" ; \
	done
