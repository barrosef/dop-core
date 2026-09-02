-- +goose Up
-- Not every event belongs to an account.
--
-- `identity.user.ensured` happens on the first login, BEFORE any account
-- exists — the user is created and only then is the personal account born.
-- Requiring account_id at that moment made sign-up impossible.
ALTER TABLE events ALTER COLUMN account_id DROP NOT NULL;

-- +goose Down
ALTER TABLE events ALTER COLUMN account_id SET NOT NULL;
