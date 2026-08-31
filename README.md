# dop-core

Núcleo da plataforma DOP: domínio, estado, transações e eventos.

**Um binário, quatro modos** (ADR-0016) — um artefato, um pipeline:

| modo | papel |
|---|---|
| `serve` | servidor gRPC do domínio |
| `worker` | consome eventos, constrói projeções e roda o **relay do outbox** |
| `sched` | polling de PRs, suspensão de sandboxes, partições, expirações |
| `launcher` | provisiona sandboxes no cluster de execução |

## A fronteira que sustenta tudo

`internal/domain` **não pode** importar `internal/adapter`, nem SDK de fornecedor.
O domínio declara PORTAS; adaptadores vivem em `internal/adapter` e são escolhidos
no composition root (`internal/app/wire.go` e `register.go`).

Isso é um **teste**, não uma convenção no README: `test/contract/architecture_test.go`
quebra o build de quem violar, nomeando arquivo, import e regra.

## A espinha de eventos

Toda mudança de estado grava o evento na **mesma transação**:

```go
postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
    if err := gravarEstado(ctx, tx); err != nil { return err }
    return postgres.Emit(ctx, tx, evento)   // mesma transação
})
```

Commit ⇒ atômico por construção; nunca "gravei mas não publiquei". O relay lê o
outbox e publica no NATS; consumidores são idempotentes porque a entrega é
ao-menos-uma-vez. Verificado em `test/integration/outbox_test.go`, que inclui a
prova negativa: transação que falha **não deixa evento órfão**.

## Estrutura

```
api/proto/          .proto — a FONTE DA VERDADE do contrato (ADR-0017)
api/gen/            código gerado (buf)
cmd/dop-core/       main: despacha os quatro modos
internal/
  domain/           entidades, serviços e PORTAS — zero imports de infra
    ports/          SecretStore · ObjectStore · IdentityProvider · EventBus
    identity/       ← implementação de REFERÊNCIA; siga este padrão
  app/              composition root, interceptores, registro
    grpc/           tradução proto ↔ domínio (camada fina, sem regra de negócio)
  adapter/          postgres (outbox, repositórios, projeções) · nats ·
                    secretstore · objectstore · identity
  platform/         logging (JSON) · ctxutil · errs · idem · config
migrations/         SQL — schema, espinha de eventos e projeções
test/contract/      testes de CONTRATO por porta + teste de arquitetura
test/integration/   espinha de eventos contra o ambiente local (build tag)
```

## Desenvolvimento

```bash
make proto             # regenera do .proto
make test              # unidade + contrato + arquitetura
make migrate           # aplica o schema no Postgres do k3d
make run-serve

# integração (exige port-forward de postgres e nats)
make test-integration
```

Ambiente local (Postgres, NATS, emuladores): ver `dop-infra`.
