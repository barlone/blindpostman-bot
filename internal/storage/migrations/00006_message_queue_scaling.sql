-- +goose Up
-- Гашение просроченных сообщений без обхода всей очереди.
CREATE INDEX messages_pending_expires_idx ON messages (expires_at)
    WHERE status = 'pending' AND expires_at IS NOT NULL;

-- Удаление старых завершённых сообщений пачками.
CREATE INDEX messages_finished_created_idx ON messages (created_at) WHERE status <> 'pending';

-- Каждое сообщение обновляется несколько раз, а затем удаляется: на стандартных настройках
-- autovacuum приходит, когда мёртвых строк уже 20% таблицы.
ALTER TABLE messages SET (autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.02);

-- +goose Down
ALTER TABLE messages RESET (autovacuum_vacuum_scale_factor, autovacuum_analyze_scale_factor);

DROP INDEX messages_finished_created_idx;
DROP INDEX messages_pending_expires_idx;
