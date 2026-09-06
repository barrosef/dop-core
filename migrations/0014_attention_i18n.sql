-- +goose Up
-- The attention box becomes localizable.
--
-- `title` and `summary` were free Portuguese text written by the domain. Text a
-- person reads cannot be frozen in one language inside the domain: the domain
-- knows WHICH sentence applies, not which words say it.
--
-- `title_key` is the stable translation key ("attention.gate_pending.title")
-- and `params` carries the values the sentence needs. `title` and `summary`
-- stay as the ENGLISH FALLBACK — for a reader with no catalogue: logs, the raw
-- API response, an operator reading the table. A client that has the catalogue
-- uses the key; one that does not still shows something.
--
-- Nullable on purpose: rows written before this migration have no key, and
-- backfilling them would mean guessing which rule produced each one.
ALTER TABLE attention_items
  ADD COLUMN IF NOT EXISTS title_key text,
  ADD COLUMN IF NOT EXISTS params    jsonb NOT NULL DEFAULT '{}'::jsonb;

-- +goose Down
-- Irreversible by design: this migration adds or corrects data the rest of the
-- schema now assumes. Rolling it back would leave a database the code cannot read.
