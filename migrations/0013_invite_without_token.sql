-- +goose Up
-- The invite stops having a secret.
--
-- Before: the link carried an opaque token and acceptance only checked its
-- hash. Whoever held the link got into the account — a BEARER credential. That
-- is why the token could not appear in an event, a projection or an e-mail,
-- which in practice stopped the invite from becoming a clickable link.
--
-- Now: acceptance requires the session's VERIFIED e-mail to be the invite's.
-- The link only has to ADDRESS the row, and `id` already does that. With no
-- secret, there is no secret to leak in `events`, `outbox`, JetStream or
-- `timeline`.
--
-- The column is dropped instead of left NULL: keeping the hash of a secret
-- nobody checks any more is keeping surface with no owner.
ALTER TABLE invites DROP COLUMN IF EXISTS token_hash;

-- +goose Down
-- Irreversible by design: this migration adds or corrects data the rest of the
-- schema now assumes. Rolling it back would leave a database the code cannot read.
