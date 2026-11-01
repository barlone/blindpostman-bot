package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type Recipient struct {
	TelegramID int64
	Token      uuid.UUID
	Alias      *string // nil, если псевдоним не задан
}

// Recipients возвращает всех получателей.
func (s *Store) Recipients(ctx context.Context) ([]Recipient, error) {
	rows, err := s.pool.Query(ctx, `SELECT tg_id, id, alias FROM recipients ORDER BY tg_id`)
	if err != nil {
		return nil, fmt.Errorf("storage: query recipients: %w", err)
	}
	recipients, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Recipient, error) {
		var r Recipient
		err := row.Scan(&r.TelegramID, &r.Token, &r.Alias)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("storage: read recipients: %w", err)
	}
	return recipients, nil
}
