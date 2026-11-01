-- +goose Up
-- По псевдониму адресуются сообщения, поэтому он должен указывать на одного получателя.
-- Псевдоним по-прежнему необязателен: строк с NULL может быть сколько угодно.
ALTER TABLE recipients ADD CONSTRAINT recipients_alias_key UNIQUE (alias);

-- +goose Down
ALTER TABLE recipients DROP CONSTRAINT recipients_alias_key;
