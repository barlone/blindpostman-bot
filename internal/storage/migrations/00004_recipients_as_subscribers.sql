-- +goose Up
-- recipients становится единственным списком получателей, subscribers вливается в него.
-- Получатель появляется здесь вручную или при регистрации по ссылке. При добавлении вручную
-- достаточно указать tg_id, остальные колонки заполнятся значениями по умолчанию.
ALTER TABLE recipients
    ADD COLUMN id           BIGSERIAL   PRIMARY KEY,
    ADD COLUMN username     TEXT        NOT NULL DEFAULT '',
    ADD COLUMN first_name   TEXT        NOT NULL DEFAULT '',
    ADD COLUMN last_name    TEXT        NOT NULL DEFAULT '',
    ADD COLUMN status       TEXT        NOT NULL DEFAULT 'active'
                            CHECK (status IN ('active', 'unsubscribed', 'blocked')),
    ADD COLUMN created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN last_sent_at TIMESTAMPTZ,
    ALTER COLUMN token SET DEFAULT gen_random_uuid(),
    ADD CONSTRAINT recipients_tg_id_key UNIQUE (tg_id),
    ADD CONSTRAINT recipients_token_key UNIQUE (token);

-- Если получатель есть в обеих таблицах, его токен и псевдоним остаются из recipients.
INSERT INTO recipients (tg_id, token, username, first_name, last_name, status,
                        created_at, updated_at, last_sent_at)
SELECT telegram_id, auth_token, username, first_name, last_name, status,
       created_at, updated_at, last_sent_at
FROM subscribers
ON CONFLICT (tg_id) DO UPDATE SET
    username     = EXCLUDED.username,
    first_name   = EXCLUDED.first_name,
    last_name    = EXCLUDED.last_name,
    status       = EXCLUDED.status,
    created_at   = EXCLUDED.created_at,
    updated_at   = EXCLUDED.updated_at,
    last_sent_at = EXCLUDED.last_sent_at;

ALTER TABLE registration_nonces DROP CONSTRAINT registration_nonces_subscriber_id_fkey;
ALTER TABLE callback_codes DROP CONSTRAINT callback_codes_subscriber_id_fkey;
ALTER TABLE messages DROP CONSTRAINT messages_subscriber_id_fkey;
ALTER TABLE events DROP CONSTRAINT events_subscriber_id_fkey;

UPDATE registration_nonces t SET subscriber_id = r.id
FROM subscribers s JOIN recipients r ON r.tg_id = s.telegram_id
WHERE t.subscriber_id = s.id;

UPDATE callback_codes t SET subscriber_id = r.id
FROM subscribers s JOIN recipients r ON r.tg_id = s.telegram_id
WHERE t.subscriber_id = s.id;

UPDATE messages t SET subscriber_id = r.id
FROM subscribers s JOIN recipients r ON r.tg_id = s.telegram_id
WHERE t.subscriber_id = s.id;

UPDATE events t SET subscriber_id = r.id
FROM subscribers s JOIN recipients r ON r.tg_id = s.telegram_id
WHERE t.subscriber_id = s.id;

ALTER TABLE registration_nonces ADD CONSTRAINT registration_nonces_subscriber_id_fkey
    FOREIGN KEY (subscriber_id) REFERENCES recipients (id) ON DELETE CASCADE;
ALTER TABLE callback_codes ADD CONSTRAINT callback_codes_subscriber_id_fkey
    FOREIGN KEY (subscriber_id) REFERENCES recipients (id) ON DELETE CASCADE;
ALTER TABLE messages ADD CONSTRAINT messages_subscriber_id_fkey
    FOREIGN KEY (subscriber_id) REFERENCES recipients (id) ON DELETE CASCADE;
ALTER TABLE events ADD CONSTRAINT events_subscriber_id_fkey
    FOREIGN KEY (subscriber_id) REFERENCES recipients (id) ON DELETE SET NULL;

DROP TABLE subscribers;

-- +goose Down
CREATE TABLE subscribers (
    id           BIGSERIAL PRIMARY KEY,
    telegram_id  BIGINT      NOT NULL UNIQUE,
    username     TEXT        NOT NULL DEFAULT '',
    first_name   TEXT        NOT NULL DEFAULT '',
    last_name    TEXT        NOT NULL DEFAULT '',
    auth_token   UUID        NOT NULL UNIQUE,
    status       TEXT        NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active', 'unsubscribed', 'blocked')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_sent_at TIMESTAMPTZ
);

-- Идентификаторы сохраняются, поэтому ссылки в других таблицах менять не нужно.
INSERT INTO subscribers (id, telegram_id, username, first_name, last_name, auth_token, status,
                         created_at, updated_at, last_sent_at)
SELECT id, tg_id, username, first_name, last_name, token, status,
       created_at, updated_at, last_sent_at
FROM recipients;

SELECT setval('subscribers_id_seq', COALESCE(max(id), 1), max(id) IS NOT NULL) FROM subscribers;

ALTER TABLE registration_nonces DROP CONSTRAINT registration_nonces_subscriber_id_fkey;
ALTER TABLE callback_codes DROP CONSTRAINT callback_codes_subscriber_id_fkey;
ALTER TABLE messages DROP CONSTRAINT messages_subscriber_id_fkey;
ALTER TABLE events DROP CONSTRAINT events_subscriber_id_fkey;

ALTER TABLE registration_nonces ADD CONSTRAINT registration_nonces_subscriber_id_fkey
    FOREIGN KEY (subscriber_id) REFERENCES subscribers (id) ON DELETE CASCADE;
ALTER TABLE callback_codes ADD CONSTRAINT callback_codes_subscriber_id_fkey
    FOREIGN KEY (subscriber_id) REFERENCES subscribers (id) ON DELETE CASCADE;
ALTER TABLE messages ADD CONSTRAINT messages_subscriber_id_fkey
    FOREIGN KEY (subscriber_id) REFERENCES subscribers (id) ON DELETE CASCADE;
ALTER TABLE events ADD CONSTRAINT events_subscriber_id_fkey
    FOREIGN KEY (subscriber_id) REFERENCES subscribers (id) ON DELETE SET NULL;

ALTER TABLE recipients
    DROP CONSTRAINT recipients_tg_id_key,
    DROP CONSTRAINT recipients_token_key,
    ALTER COLUMN token DROP DEFAULT,
    DROP COLUMN id,
    DROP COLUMN username,
    DROP COLUMN first_name,
    DROP COLUMN last_name,
    DROP COLUMN status,
    DROP COLUMN created_at,
    DROP COLUMN updated_at,
    DROP COLUMN last_sent_at;
