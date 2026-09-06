-- +goose Up
-- A revoked share is not a permanent one.
--
-- flow_share_unico (migration 0021) was a PLAIN unique constraint on
-- (publication_id, to_account_id), and a plain constraint counts every row —
-- revoked or not. That made a revocation permanent by accident: once an
-- account's grant on a publication was revoked, the row occupying that key
-- meant the account could never be granted that publication again. Nothing in
-- the design says revocation is forever; revoke-then-share-again is an
-- ordinary thing to want, the same way withdrawing a publication does not stop
-- it from being published again.
--
-- The fix is the constraint's SCOPE, not its removal: two ACTIVE grants on the
-- same (publication, account) still make no sense — a revoked one sitting
-- alongside a new one is exactly the audit trail a re-grant should leave
-- behind.
ALTER TABLE flow_shares DROP CONSTRAINT flow_share_unico;
CREATE UNIQUE INDEX flow_share_unico ON flow_shares (publication_id, to_account_id)
  WHERE revoked_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS flow_share_unico;
ALTER TABLE flow_shares ADD CONSTRAINT flow_share_unico UNIQUE (publication_id, to_account_id);
