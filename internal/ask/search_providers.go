package ask

import (
	"context"
	"errors"
	"fmt"

	"github.com/archer-developer/miranda-llm/embedding"

	"github.com/archer-developer/miranda-medical-card/internal/search"
	"github.com/archer-developer/miranda-medical-card/internal/storage"
)

// --- Document (FTS) ---

// DocumentProvider mirrors docs/architecture/04-search.md §12/§13 — free
// text not captured by any structured entity (subjective complaints,
// doctor's free-form remarks), searched via FTS5 rather than re-reading
// every document.
type DocumentProvider struct {
	fts       storage.FTSRepository
	documents storage.DocumentRepository
}

func NewDocumentProvider(fts storage.FTSRepository, documents storage.DocumentRepository) *DocumentProvider {
	return &DocumentProvider{fts: fts, documents: documents}
}

func (p *DocumentProvider) Metadata() ProviderMetadata {
	return ProviderMetadata{
		Name: "documents",
		Description: "Полнотекстовый поиск по распознанному тексту документов, их кратким описаниям и рекомендациям " +
			"врачей — то, что не превратилось в структурированные данные (жалобы, свободные комментарии врача, " +
			"рекомендации по питанию/образу жизни). Укажите searchQuery — несколько ключевых слов по-русски; " +
			"результат покажет только короткий фрагмент вокруг совпадения вместе с id документа. Чтобы получить " +
			"полное summary этого документа целиком (включая полный список рекомендаций, не обрывок), вызовите " +
			"documents ещё раз с documentId — этим самым id.",
	}
}

func (p *DocumentProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	if req.DocumentID != "" {
		return p.collectByDocumentID(ctx, req)
	}
	if req.Query == "" {
		return nil, nil
	}
	results, err := p.fts.SearchDocuments(ctx, req.UserID, req.Query, limitOrDefault(req.Limit))
	if err != nil {
		return nil, fmt.Errorf("ask: document provider: %w", err)
	}
	chunks := make([]KnowledgeChunk, len(results))
	for i, r := range results {
		chunks[i] = KnowledgeChunk{
			Source: "documents", Title: r.Title, Content: r.Snippet,
			Confidence: 0.82, DocumentID: r.DocumentID,
		}
	}
	return chunks, nil
}

// collectByDocumentID returns doc.Summary in full — unlike SearchDocuments'
// FTS snippet (a short window around a keyword match, see fts.go), Summary
// is the whole mechanically-assembled fact list (docs/domain/03-files-and-
// documents.md §3), including every recommendation, not just the ones
// close enough to a search term to land in a 40-token snippet. documents.Get
// is already scoped to (id, userID) in SQL (see storage.DocumentRepository),
// so a hallucinated or cross-household id just resolves to "not found"
// rather than leaking another user's document.
func (p *DocumentProvider) collectByDocumentID(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	doc, err := p.documents.Get(ctx, req.DocumentID, req.UserID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ask: document provider: get %s: %w", req.DocumentID, err)
	}
	return []KnowledgeChunk{{
		Source: "documents", Title: doc.Title, Content: doc.Summary,
		Confidence: 1.0, DocumentID: doc.ID,
	}}, nil
}

// --- Embeddings (semantic search) ---

// EmbeddingProvider mirrors docs/architecture/04-search.md §14 — semantic
// search over document summaries and self-reported event text, for
// questions with no exact keyword match (e.g. "когда мне было плохо").
//
// A matched Embedding row is only a (sourceType, sourceId, score) triple —
// no text of its own (see storage.Embedding's doc comment) — so this
// provider also reads the underlying MedicalDocument.Summary or
// SelfReportedEvent.RawText/Description to build a chunk with actual
// Content, not just an identifier.
type EmbeddingProvider struct {
	repo         storage.EmbeddingRepository
	documents    storage.DocumentRepository
	events       storage.SelfReportedEventRepository
	embedder     embedding.Embedder
	modelVersion string
}

func NewEmbeddingProvider(repo storage.EmbeddingRepository, documents storage.DocumentRepository, events storage.SelfReportedEventRepository, embedder embedding.Embedder, modelVersion string) *EmbeddingProvider {
	return &EmbeddingProvider{repo: repo, documents: documents, events: events, embedder: embedder, modelVersion: modelVersion}
}

func (p *EmbeddingProvider) Metadata() ProviderMetadata {
	return ProviderMetadata{
		Name: "embeddings",
		Description: "Семантический поиск по смыслу, а не по точным словам — находит похожие описания и косвенные " +
			"упоminания, даже если нужное слово не встречается буквально (например 'когда мне было плохо' вместо " +
			"конкретного диагноза). Укажите searchQuery. НЕ используйте для точных терминов (названий анализов, " +
			"препаратов) — для них есть структурированные Providers, которые всегда точнее.",
	}
}

func (p *EmbeddingProvider) Collect(ctx context.Context, req KnowledgeRequest) ([]KnowledgeChunk, error) {
	if req.Query == "" {
		return nil, nil
	}
	queryVector, err := p.embedder.Embed(ctx, req.Query)
	if err != nil {
		return nil, fmt.Errorf("ask: embedding provider: embed query: %w", err)
	}
	embeddings, err := p.repo.ListByUser(ctx, req.UserID, p.modelVersion)
	if err != nil {
		return nil, fmt.Errorf("ask: embedding provider: %w", err)
	}
	scored := search.CosineSearch(queryVector, embeddings, limitOrDefault(req.Limit))

	chunks := make([]KnowledgeChunk, 0, len(scored))
	for _, s := range scored {
		// A near-zero/negative cosine similarity means "unrelated," not "a
		// weak match" — including it would just add noise to the context,
		// see docs/architecture/04-search.md §3 "Минимизировать объём
		// контекста".
		if s.Score <= 0.5 {
			continue
		}
		chunk, ok := p.resolveChunk(ctx, req.UserID, s)
		if !ok {
			continue
		}
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}

// resolveChunk looks up s's underlying text — a MedicalDocument.Summary or
// a SelfReportedEvent's RawText/Description — and reports ok=false if the
// source has since been deleted (the embedding row would be stale; skip it
// rather than surface an empty chunk) or is an unrecognized source type.
func (p *EmbeddingProvider) resolveChunk(ctx context.Context, userID string, s search.Scored) (KnowledgeChunk, bool) {
	switch s.SourceType {
	case "summary":
		doc, err := p.documents.Get(ctx, s.SourceID, userID)
		if err != nil {
			return KnowledgeChunk{}, false
		}
		title := doc.Title
		if title == "" {
			title = "Документ"
		}
		return KnowledgeChunk{
			Source: "embeddings", Title: title, Content: doc.Summary,
			Confidence: 0.74 * s.Score, DocumentID: doc.ID,
		}, doc.Summary != ""
	case "self_reported_event":
		event, err := p.events.Get(ctx, s.SourceID, userID)
		if err != nil {
			return KnowledgeChunk{}, false
		}
		content := event.Description
		if content == "" {
			content = event.RawText
		}
		return KnowledgeChunk{
			Source: "embeddings", Title: "Запись пользователя", Content: content,
			Confidence: 0.74 * s.Score, EventID: event.ID,
		}, content != ""
	default:
		return KnowledgeChunk{}, false
	}
}
