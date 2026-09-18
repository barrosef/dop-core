# dop-core

The DOP platform's core: domain, state, transactions and events.

Part of the [DOP platform](https://dop-t.com) — what it is, how it is built and where it stands: **[dop-t.com](https://dop-t.com)**.

**One binary, four modes** (ADR-0012) — one artifact, one pipeline:

| mode | role |
|---|---|
| `serve` | the domain's gRPC server |
| `worker` | consumes events, builds projections and runs the **outbox relay** |
| `sched` | PR polling, sandbox suspension, partitions, expirations |
| `launcher` | provisions sandboxes in the execution cluster |

## The boundary that holds everything up

`internal/domain` **may not** import `internal/adapter`, nor any provider SDK.
The domain declares PORTS; adapters live in `internal/adapter` and are chosen in
the composition root (`internal/app/wire.go` and `register.go`).

This is a **test**, not a convention in the README:
`test/contract/architecture_test.go` breaks the build of whoever violates it,
naming the file, the import and the rule.

## The event spine

Every state change writes the event in the **same transaction**:

```go
postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
    if err := writeState(ctx, tx); err != nil { return err }
    return postgres.Emit(ctx, tx, event)   // the same transaction
})
```

Commit ⇒ atomic by construction; never "I wrote but did not publish". The relay
reads the outbox and publishes to NATS; consumers are idempotent because
delivery is at-least-once. Verified in `test/integration/outbox_test.go`, which
includes the negative proof: a transaction that fails **leaves no orphan
event**.

## Structure

```
api/proto/          .proto — the contract's SOURCE OF TRUTH (ADR-0013)
api/gen/            generated code (buf)
cmd/dop-core/       main: dispatches the four modes
internal/
  domain/           entities, services and PORTS — zero infrastructure imports
    ports/          SecretStore · ObjectStore · IdentityProvider · EventBus
    identity/       ← the REFERENCE implementation; follow this pattern
  app/              composition root, interceptors, registration
    grpc/           proto ↔ domain translation (a thin layer, no business rule)
  adapter/          postgres (outbox, repositories, projections) · nats ·
                    secretstore · objectstore · identity
  platform/         logging (JSON) · ctxutil · errs · idem · config
migrations/         SQL — schema, event spine and projections
test/contract/      CONTRACT tests per port + the architecture test
test/integration/   the event spine against the local environment (build tag)
```

## Development

```bash
make proto             # regenerate from the .proto files
make test              # unit + contract + architecture
make migrate           # apply the schema to k3d's Postgres
make run-serve

# integration (requires a port-forward of postgres and nats)
make test-integration
```

The local environment (Postgres, NATS, emulators): see `dop-infra`.
