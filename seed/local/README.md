Seeds for the local development environment. They run only when the worker
starts with `DOP_SEED_PROFILE=local`, after the root seeds. Same contract as
the root: idempotent, upsert by key, safe to re-run.
