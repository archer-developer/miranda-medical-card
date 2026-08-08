package storage

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
)

// Embedding mirrors docs/domain/10-medical-knowledge-and-embedding.md §3.
//
// Scope note: this implementation only ever writes SourceType "summary"
// (one embedding per MedicalDocument, vectorizing MedicalDocument.Summary)
// and "self_reported_event" (one per SelfReportedEvent, vectorizing
// RawText+Description) — see docs/architecture/04-search.md §14's own
// recommendation that self-reported free text is "один из лучших
// кандидатов" for Embedding Search. The domain doc additionally lists
// diagnosis/procedure/timeline_event/recommendation as embeddable
// (docs/architecture/02-processing-pipeline.md §10) — deliberately not
// implemented here; one embedding per document already covers Document
// Search's main use case (docs/architecture/04-search.md §12/§14), and
// per-entity embeddings are a documented future enhancement, not a gap in
// what's already promised.
type Embedding struct {
	ID           string
	UserID       string
	SourceType   string
	SourceID     string
	Provider     string
	ModelVersion string
	Vector       []float32
	CreatedAt    time.Time
}

// EmbeddingRepository mirrors docs/domain/10-medical-knowledge-and-embedding.md
// §3 "Repository", plus RemoveBySource for self-reported-event embeddings
// (RemoveByDocument only matches SourceType "summary" rows, see Embedding's
// doc comment — a self-reported event has no documentId to key off of, the
// same reason TimelineRepository needed RemoveBySourceEntity alongside
// RemoveByDocument).
type EmbeddingRepository interface {
	Add(ctx context.Context, e Embedding) error
	// ListByUser returns every embedding for userID built with modelVersion
	// — callers do in-memory cosine search over the result (see
	// internal/search), matching miranda-diary's Store.Search precedent.
	ListByUser(ctx context.Context, userID, modelVersion string) ([]Embedding, error)
	RemoveByDocument(ctx context.Context, documentID string) error
	RemoveBySource(ctx context.Context, sourceType, sourceID string) error
}

type sqliteEmbeddingRepository struct {
	db *sql.DB
}

// NewEmbeddingRepository builds an EmbeddingRepository backed by s.
func NewEmbeddingRepository(s *Store) EmbeddingRepository {
	return &sqliteEmbeddingRepository{db: s.db}
}

func (r *sqliteEmbeddingRepository) Add(ctx context.Context, e Embedding) error {
	if e.ID == "" {
		e.ID = "emb_" + uuid.New().String()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO embeddings (id, user_id, source_type, source_id, provider, model_version, vector, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.UserID, e.SourceType, e.SourceID, e.Provider, e.ModelVersion, encodeVector(e.Vector), e.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("storage: add embedding: %w", err)
	}
	return nil
}

func (r *sqliteEmbeddingRepository) ListByUser(ctx context.Context, userID, modelVersion string) ([]Embedding, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, user_id, source_type, source_id, provider, model_version, vector, created_at FROM embeddings WHERE user_id = ? AND model_version = ?`,
		userID, modelVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: list embeddings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []Embedding
	for rows.Next() {
		var e Embedding
		var vectorBlob []byte
		var createdAt int64
		if err := rows.Scan(&e.ID, &e.UserID, &e.SourceType, &e.SourceID, &e.Provider, &e.ModelVersion, &vectorBlob, &createdAt); err != nil {
			return nil, fmt.Errorf("storage: scan embedding: %w", err)
		}
		vector, err := decodeVector(vectorBlob)
		if err != nil {
			// A corrupt vector shouldn't fail the whole search — skip it,
			// mirroring miranda-diary's Store.Search decodeEmbedding
			// handling.
			continue
		}
		e.Vector = vector
		e.CreatedAt = time.Unix(createdAt, 0).UTC()
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: iterate embeddings: %w", err)
	}
	return result, nil
}

func (r *sqliteEmbeddingRepository) RemoveByDocument(ctx context.Context, documentID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM embeddings WHERE source_type = 'summary' AND source_id = ?`, documentID)
	if err != nil {
		return fmt.Errorf("storage: remove embeddings by document: %w", err)
	}
	return nil
}

func (r *sqliteEmbeddingRepository) RemoveBySource(ctx context.Context, sourceType, sourceID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM embeddings WHERE source_type = ? AND source_id = ?`, sourceType, sourceID)
	if err != nil {
		return fmt.Errorf("storage: remove embeddings by source: %w", err)
	}
	return nil
}

// encodeVector/decodeVector pack a float32 slice as little-endian bytes —
// identical scheme to miranda-diary's encodeEmbedding/decodeEmbedding (see
// that package for the reference implementation this doc comment points
// to, per docs/domain/10-medical-knowledge-and-embedding.md §3's own
// "vector: blob ... little-endian float32, см. miranda-diary's
// encodeEmbedding").
func encodeVector(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func decodeVector(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("storage: invalid vector blob length %d", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}
