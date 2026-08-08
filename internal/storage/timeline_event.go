package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/archer-developer/miranda-medical-card/internal/timeline"
)

// TimelineFilter mirrors docs/domain/04-timeline.md §5 and
// docs/mcp/06-timeline.md §4.
type TimelineFilter struct {
	From  *time.Time
	To    *time.Time
	Types []string
	// Limit caps the number of returned events, most recent first (see
	// docs/mcp/06-timeline.md §8 "по умолчанию... от новых к старым"). Zero
	// means no limit.
	Limit int
}

// TimelineRepository mirrors docs/domain/04-timeline.md §5, plus
// RemoveBySourceEntity — needed by SelfReportedEvent deletion (see
// docs/domain/12-self-reported-events.md §8: "Delete TimelineEvent (по
// sourceEntityId)"), which has no documentId to key off of the way
// document-derived events do.
type TimelineRepository interface {
	Add(ctx context.Context, e timeline.Event) error
	// List returns events for userID matching filter, newest first (see
	// TimelineFilter.Limit).
	List(ctx context.Context, userID string, filter TimelineFilter) ([]timeline.Event, error)
	RemoveByDocument(ctx context.Context, documentID string) error
	RemoveBySourceEntity(ctx context.Context, sourceEntityType, sourceEntityID string) error
}

type sqliteTimelineRepository struct {
	db *sql.DB
}

// NewTimelineRepository builds a TimelineRepository backed by s.
func NewTimelineRepository(s *Store) TimelineRepository {
	return &sqliteTimelineRepository{db: s.db}
}

const timelineEventSelectColumns = `SELECT id, user_id, date, type, title, summary, document_id, source_entity_type, source_entity_id`

func (r *sqliteTimelineRepository) Add(ctx context.Context, e timeline.Event) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO timeline_events (id, user_id, date, type, title, summary, document_id, source_entity_type, source_entity_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.UserID, e.Date.Unix(), e.Type, e.Title, e.Summary, e.DocumentID, e.SourceEntityType, e.SourceEntityID,
	)
	if err != nil {
		return fmt.Errorf("storage: add timeline event: %w", err)
	}
	return nil
}

func (r *sqliteTimelineRepository) List(ctx context.Context, userID string, filter TimelineFilter) ([]timeline.Event, error) {
	query := timelineEventSelectColumns + ` FROM timeline_events WHERE user_id = ?`
	args := []any{userID}
	if filter.From != nil {
		query += ` AND date >= ?`
		args = append(args, filter.From.Unix())
	}
	if filter.To != nil {
		query += ` AND date <= ?`
		args = append(args, filter.To.Unix())
	}
	if len(filter.Types) > 0 {
		query += ` AND type IN (` + placeholders(len(filter.Types)) + `)`
		for _, t := range filter.Types {
			args = append(args, t)
		}
	}
	query += ` ORDER BY date DESC`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: list timeline events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []timeline.Event
	for rows.Next() {
		var e timeline.Event
		var date int64
		if err := rows.Scan(&e.ID, &e.UserID, &date, &e.Type, &e.Title, &e.Summary, &e.DocumentID, &e.SourceEntityType, &e.SourceEntityID); err != nil {
			return nil, fmt.Errorf("storage: scan timeline event: %w", err)
		}
		e.Date = time.Unix(date, 0).UTC()
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate timeline events: %w", err)
	}
	return result, nil
}

func (r *sqliteTimelineRepository) RemoveByDocument(ctx context.Context, documentID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM timeline_events WHERE document_id = ?`, documentID)
	if err != nil {
		return fmt.Errorf("storage: remove timeline events by document: %w", err)
	}
	return nil
}

func (r *sqliteTimelineRepository) RemoveBySourceEntity(ctx context.Context, sourceEntityType, sourceEntityID string) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM timeline_events WHERE source_entity_type = ? AND source_entity_id = ?`,
		sourceEntityType, sourceEntityID,
	)
	if err != nil {
		return fmt.Errorf("storage: remove timeline events by source entity: %w", err)
	}
	return nil
}

// placeholders returns n comma-separated "?" placeholders for an IN clause.
func placeholders(n int) string {
	s := make([]byte, 0, n*2-1)
	for i := 0; i < n; i++ {
		if i > 0 {
			s = append(s, ',')
		}
		s = append(s, '?')
	}
	return string(s)
}
