-- +goose Up
-- Таблица messages становится очередью доставки.
ALTER TABLE messages
    ADD COLUMN disable_notification BOOLEAN     NOT NULL DEFAULT false,
    ADD COLUMN attempts             INT         NOT NULL DEFAULT 0,
    ADD COLUMN next_attempt_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Аренда: пока locked_until в будущем, сообщение обрабатывает какой-то воркер.
    ADD COLUMN locked_until         TIMESTAMPTZ,
    -- После этого момента алерт уже неактуален и не доставляется.
    ADD COLUMN expires_at           TIMESTAMPTZ,
    ADD COLUMN updated_at           TIMESTAMPTZ NOT NULL DEFAULT now();

ALTER TABLE messages DROP CONSTRAINT messages_status_check;
ALTER TABLE messages ADD CONSTRAINT messages_status_check
    CHECK (status IN ('pending', 'sent', 'failed', 'expired'));

-- Выбор «головы» очереди каждого подписчика.
CREATE INDEX messages_pending_idx ON messages (subscriber_id, id) WHERE status = 'pending';

-- Для соблюдения интервала между сообщениями в один чат.
ALTER TABLE subscribers ADD COLUMN last_sent_at TIMESTAMPTZ;

-- Будим диспетчер сразу после постановки сообщения в очередь.
-- +goose StatementBegin
CREATE FUNCTION notify_message_queued() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('blind_postman_messages', '');
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER messages_queued
    AFTER INSERT ON messages
    FOR EACH STATEMENT EXECUTE FUNCTION notify_message_queued();

-- +goose Down
DROP TRIGGER messages_queued ON messages;
DROP FUNCTION notify_message_queued();

ALTER TABLE subscribers DROP COLUMN last_sent_at;

DROP INDEX messages_pending_idx;

-- В старой схеме статуса expired нет, ближайший к нему — failed.
UPDATE messages SET status = 'failed' WHERE status = 'expired';
ALTER TABLE messages DROP CONSTRAINT messages_status_check;
ALTER TABLE messages ADD CONSTRAINT messages_status_check
    CHECK (status IN ('pending', 'sent', 'failed'));

ALTER TABLE messages
    DROP COLUMN disable_notification,
    DROP COLUMN attempts,
    DROP COLUMN next_attempt_at,
    DROP COLUMN locked_until,
    DROP COLUMN expires_at,
    DROP COLUMN updated_at;
