-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- How the agent's session was AUTHENTICATED (P-23 phase 1).
--
-- Phase 1 measures consumption on the dev's own subscription; phase 2 measures
-- it on the platform's API key. The same numbers mean different things in each,
-- and "which one was this?" cannot be inferred from an environment variable —
-- a stray ANTHROPIC_API_KEY silently makes the tool bill the key instead of the
-- subscription, and the measurement would be about the wrong thing with nothing
-- saying so.
--
-- So the collector records the OBSERVED state of the agent: what the tool itself
-- reports about its own authentication, once, at startup.
--
-- What is deliberately NOT here: the e-mail, the organization's name and id of
-- the Anthropic account. The platform's own account already says whose demand
-- this is; a second identity adds nothing to a measurement and adds a surface.
-- These columns are about the METHOD, not about the person.
-- ════════════════════════════════════════════════════════════════════════════

ALTER TABLE agent_sessions
  ADD COLUMN auth_method       text NOT NULL DEFAULT '',  -- claude.ai | apiKey | …
  ADD COLUMN api_provider      text NOT NULL DEFAULT '',  -- firstParty | bedrock | …
  ADD COLUMN subscription_type text NOT NULL DEFAULT '',  -- max | pro | '' when billed by key
  ADD COLUMN api_key_source    text NOT NULL DEFAULT '';  -- where a key came from, when one did

-- The question this table exists to answer per method: a comparison between
-- phase 1 and phase 2 reads by this column.
CREATE INDEX agent_sessions_auth_idx ON agent_sessions (auth_method, subscription_type);

-- +goose Down
DROP INDEX IF EXISTS agent_sessions_auth_idx;
ALTER TABLE agent_sessions
  DROP COLUMN IF EXISTS auth_method,
  DROP COLUMN IF EXISTS api_provider,
  DROP COLUMN IF EXISTS subscription_type,
  DROP COLUMN IF EXISTS api_key_source;
