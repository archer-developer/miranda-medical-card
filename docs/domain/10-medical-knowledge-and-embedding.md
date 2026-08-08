# MedicalKnowledge, Embedding

> **Статус:** Draft
>
> **Раздел:** Domain
>
> **Версия:** 1.0
>
> **Связанные документы**
>
> - 01-overview.md
> - ../architecture/03-knowledge-providers.md
> - ../architecture/04-search.md

---

# 1. Назначение

Обе сущности поддерживают поиск и построение контекста для LLM (см. `../architecture/04-search.md`), но не являются самостоятельными источниками медицинских знаний — они производны от Domain Entities и документов.

---

# 2. MedicalKnowledge

Промежуточная сущность между Processing Pipeline и Knowledge Providers (см. `01-overview.md` §4). Не предназначена для прямого использования через MCP.

## Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`know_...`) | ✅ | Идентификатор. |
| `userId` | string | ✅ | Владелец. |
| `documentId` | string | ✅ | Документ-источник. |
| `sourceType` | enum (`summary`, `diagnosis`, `procedure`, `timeline_event`, `recommendation`) | ✅ | Какая часть документа/сущности представлена этим фрагментом знаний — см. `../architecture/02-processing-pipeline.md` §10. |
| `sourceId` | string | ❌ | Идентификатор исходной сущности (`Diagnosis.id`, `TimelineEvent.id`, ...), если `sourceType` ссылается на конкретную сущность, а не на документ целиком. |
| `content` | text | ✅ | Человекочитаемый текст фрагмента — то, что в итоге станет `Knowledge Chunk.Content` (см. `../architecture/03-knowledge-providers.md` §12), не сырые внутренние структуры. |

## Инварианты

- Не возвращается напрямую ни одним MCP Tool — используется только Knowledge Providers как один из входов при построении `Knowledge Chunk`.
- Полностью производна: удаление или пересчёт `MedicalDocument`/породившей сущности удаляет соответствующие `MedicalKnowledge`.

## Repository

`KnowledgeRepository`: `Add`, `ListByDocument(documentId)`, `RemoveByDocument(documentId)`.

---

# 3. Embedding

Векторное представление фрагмента знаний, используемое исключительно Search Engine (Embedding Search, см. `../architecture/04-search.md` §13). Не содержит бизнес-логики.

## Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`emb_...`) | ✅ | Идентификатор. |
| `userId` | string | ✅ | Владелец — каждый поиск обязан фильтровать по этому полю. |
| `sourceType` | enum (`summary`, `diagnosis`, `procedure`, `timeline_event`, `recommendation`) | ✅ | Что именно эмбеддится — см. `../architecture/02-processing-pipeline.md` §10 (Summary, Diagnosis, Procedure, Timeline Event, Recommendations могут иметь собственные Embeddings). |
| `sourceId` | string | ✅ | Идентификатор исходной сущности/`MedicalKnowledge`. |
| `provider` | string | ✅ | Провайдер эмбеддинга (например `gemini`). |
| `modelVersion` | string | ✅ | Версия модели — критично для инвалидации при смене модели (см. §4). |
| `vector` | blob | ✅ | Бинарное представление вектора (little-endian float32, см. `miranda-diary`'s `encodeEmbedding` за референсной реализацией). |
| `createdAt` | timestamp | ✅ | Момент построения. |

## Инварианты

- Векторы, построенные разными `modelVersion`, находятся в несовместимых пространствах — сравнение допустимо только между эмбеддингами одной модели (см. `miranda-llm/embedding.GeminiEmbedder`'s doc comment). Смена модели требует полного перестроения Embeddings, а не частичного добавления новых версий.
- Полностью производна и может быть удалена и перестроена в любой момент без потери информации (источник истины — сущность/документ, из которого построен эмбеддинг).
- Поиск (`Embedding Search`, см. `../architecture/04-search.md` §13) всегда выполняется в памяти по эмбеддингам одного `userId`, а не по всей базе — см. `../architecture/06-storage.md` §11 "Индексы".

## Repository

`EmbeddingRepository`

```text
Add(e Embedding) error
ListByUser(userId string, modelVersion string) ([]Embedding, error) // для in-memory cosine search
RemoveByDocument(documentId string) error
```
