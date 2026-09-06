-- +goose Up
-- The account's DEFAULT revocation policy (flow sharing spec §3.2).
--
-- It is a default and nothing more: the value that matters is the one COPIED
-- onto the grant when a share is made. Reading the account at revocation time
-- would let the publisher change the terms after somebody accepted them — you
-- adopt under `prospective` and get terminated under `terminate`, with a demand
-- stopping mid-flight.
CREATE TYPE revocation_policy AS ENUM ('prospective', 'drain', 'terminate');

ALTER TABLE accounts
  ADD COLUMN default_revocation_policy revocation_policy NOT NULL DEFAULT 'prospective';

-- +goose Down
ALTER TABLE accounts DROP COLUMN IF EXISTS default_revocation_policy;
DROP TYPE IF EXISTS revocation_policy;
