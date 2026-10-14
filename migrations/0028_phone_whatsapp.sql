-- +goose Up
-- ════════════════════════════════════════════════════════════════════════════
-- Whether the contact phone is also the person's WhatsApp.
--
-- A column on `users`, next to `phone`, because it describes THAT number: the
-- journey's contact step asks it right where the number is typed, and a
-- notification channel that reaches people on WhatsApp reads it from here.
-- It is a statement by the person, not a verification — `phone_verified_at`
-- says the number answered an SMS; this says the person uses it on WhatsApp.
-- The two are independent: changing the number keeps the flag, confirming
-- the number does not set it.
-- ════════════════════════════════════════════════════════════════════════════

ALTER TABLE users ADD COLUMN phone_whatsapp boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE users DROP COLUMN phone_whatsapp;
