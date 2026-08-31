-- +goose Up
-- Nem todo evento pertence a uma conta.
--
-- `identity.user.ensured` acontece no primeiro login, ANTES de existir qualquer
-- conta — o usuário é criado e só então a conta pessoal nasce. Exigir
-- account_id nessa hora tornava o cadastro impossível.
ALTER TABLE events ALTER COLUMN account_id DROP NOT NULL;

-- +goose Down
ALTER TABLE events ALTER COLUMN account_id SET NOT NULL;
