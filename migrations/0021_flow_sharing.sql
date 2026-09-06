-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Sharing a flow between accounts.
--
-- Three invariants shape this schema:
--
--  1. A PUBLICATION FREEZES A VERSION. Publishing again publishes a newer one;
--     it never mutates what somebody already derived.
--  2. ADOPTION IS A COPY. The derived flow is an ordinary row in `flows`, with
--     the adopter's account_id. Nothing here lets one account read another's
--     flows — the only crossing is resolving a reference, and a `flow_shares`
--     row authorises it.
--  3. THE SAME FACT IS RECORDED ON BOTH SIDES. Provenance lives on the copy
--     (`flows.origin_*`) and the derivation lives with the publisher
--     (`flow_adoptions`), so neither side has to scan across accounts.
-- ════════════════════════════════════════════════════════════════════════════

CREATE TABLE flow_publications (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  flow_id     uuid NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
  account_id  uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  -- The slug completes @handle/slug. The handle is not stored: it lives on the
  -- account and renaming an account must not orphan its publications.
  slug        text NOT NULL,
  version     int  NOT NULL,
  notes       text,
  withdrawn_at timestamptz,
  published_by uuid REFERENCES users(id),
  published_at timestamptz NOT NULL DEFAULT now(),

  -- (flow_id, version) has to exist: publishing a version that was never
  -- written is how a reference comes to resolve to nothing.
  FOREIGN KEY (flow_id, version) REFERENCES flow_versions(flow_id, version),
  -- One publication per (account, slug, version). Publishing the same version
  -- twice is a retry, not a second publication.
  CONSTRAINT flow_publication_unica UNIQUE (account_id, slug, version)
);

-- Resolving `@handle/slug` to the LATEST published version, in one index scan.
CREATE INDEX flow_publications_lookup
  ON flow_publications (account_id, slug, version DESC)
  WHERE withdrawn_at IS NULL;

CREATE TABLE flow_shares (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  publication_id uuid NOT NULL REFERENCES flow_publications(id) ON DELETE CASCADE,
  to_account_id  uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  -- STAMPED at grant time from the publisher account's default. It is not read
  -- from the account at revocation time, so that the terms somebody accepted
  -- cannot be changed under them afterwards.
  revocation_policy revocation_policy NOT NULL,
  granted_by     uuid REFERENCES users(id),
  granted_at     timestamptz NOT NULL DEFAULT now(),
  revoked_at     timestamptz,
  CONSTRAINT flow_share_unico UNIQUE (publication_id, to_account_id)
);

CREATE INDEX flow_shares_to_account ON flow_shares (to_account_id) WHERE revoked_at IS NULL;

-- The PUBLISHER's index of where its flow went. It is the outbound half of the
-- same fact `flows.origin_*` records on the copy.
CREATE TABLE flow_adoptions (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  publication_id uuid NOT NULL REFERENCES flow_publications(id) ON DELETE CASCADE,
  version        int  NOT NULL,
  by_account_id  uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  -- The derived flow, in the OTHER account. No FK on purpose: a cascade from
  -- here would let one account's delete reach into another's rows.
  flow_id        uuid NOT NULL,
  derived_at     timestamptz NOT NULL DEFAULT now(),
  revoked_at     timestamptz
);

CREATE INDEX flow_adoptions_by_publication ON flow_adoptions (publication_id);

-- Provenance on the COPY, and the revocation mark.
ALTER TABLE flows
  ADD COLUMN origin_ref        text,
  ADD COLUMN origin_version    int,
  ADD COLUMN origin_adopted_at timestamptz,
  ADD COLUMN revoked_at        timestamptz,
  -- Provenance is all-or-nothing: a flow that says where it came from says which
  -- version it came from. Half a provenance is worse than none, because it looks
  -- answerable.
  ADD CONSTRAINT flow_origem_completa CHECK (
    (origin_ref IS NULL     AND origin_version IS NULL     AND origin_adopted_at IS NULL) OR
    (origin_ref IS NOT NULL AND origin_version IS NOT NULL AND origin_adopted_at IS NOT NULL)
  );

-- The pin: which version of an inherited flow this account is on.
--
-- It exists because inheritance that crosses an OWNERSHIP boundary must not
-- change under the account that inherits it (spec §2.5). Inheritance inside one
-- account stays live and has no row here.
CREATE TABLE account_flow_pins (
  account_id uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  flow_id    uuid NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
  version    int  NOT NULL,
  pinned_by  uuid REFERENCES users(id),
  pinned_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, flow_id),
  FOREIGN KEY (flow_id, version) REFERENCES flow_versions(flow_id, version)
);

-- +goose Down
DROP TABLE account_flow_pins;
ALTER TABLE flows
  DROP CONSTRAINT flow_origem_completa,
  DROP COLUMN revoked_at,
  DROP COLUMN origin_adopted_at,
  DROP COLUMN origin_version,
  DROP COLUMN origin_ref;
DROP TABLE flow_adoptions;
DROP TABLE flow_shares;
DROP TABLE flow_publications;
