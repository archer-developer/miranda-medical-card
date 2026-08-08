# Timeline, TimelineEvent

> **Статус:** Draft
>
> **Раздел:** Domain
>
> **Версия:** 1.0
>
> **Связанные документы**
>
> - 01-overview.md
> - ../architecture/02-processing-pipeline.md
> - ../mcp/06-timeline.md

---

# 1. Назначение

`Timeline` — материализованное хронологическое представление истории болезни пользователя, состоящее из `TimelineEvent`. Не является источником истины (источник истины — Domain Entities, из которых событие построено), а оптимизированной для чтения проекцией.

---

# 2. TimelineEvent

## Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`evt_...`) | ✅ | Идентификатор события. Стабилен — используется UI для навигации (см. `../mcp/06-timeline.md` §15). |
| `userId` | string | ✅ | Владелец. |
| `date` | date | ✅ | Дата события. |
| `type` | `TimelineEventType` (VO) | ✅ | Тип события — см. Value Objects (`11-value-objects.md`) за полным перечнем. |
| `title` | string | ✅ | Краткое название для отображения. |
| `summary` | text | ✅ | Краткое описание, только факты — без медицинских выводов (см. `../mcp/06-timeline.md` §9). |
| `documentId` | string | ❌ | Документ-источник, если событие связано с конкретным документом. |
| `sourceEntityType` | enum (`medication`, `diagnosis`, `procedure`, `lab_result`, `vital_sign`, `document`) | ❌ | Тип доменной сущности того же документа, породившей это событие. |
| `sourceEntityId` | string | ❌ | Идентификатор этой сущности. Используется только для перестроения/удаления события вместе с сущностью — не образует связь между документами (см. §4). |

## Пример

```json
{
  "id": "evt_01J...",
  "date": "2025-03-12",
  "type": "lab_result",
  "title": "Общий анализ крови",
  "summary": "Выявлено небольшое повышение ALT.",
  "documentId": "doc_01J...",
  "sourceEntityType": "lab_result",
  "sourceEntityId": "lab_01J..."
}
```

---

# 3. Timeline

`Timeline` как отдельная сущность-агрегат не хранит собственных полей сверх коллекции `TimelineEvent` пользователя — это логическая группировка, а не таблица.

```text
Timeline(userId) = { события: TimelineEvent[] | событие.userId == userId }
```

Фильтрация по периоду/типу (`from`, `to`, `types`, `limit` — см. `../mcp/06-timeline.md` §4) выполняется на уровне запроса к Repository, а не над всей коллекцией в памяти.

---

# 4. Инварианты

- Каждое `TimelineEvent`, у которого задан `sourceEntityId`, привязано **строго к сущности из того же документа** (`documentId`), из которого оно построено — это внутренняя (intra-document) связь, необходимая только для перестроения/удаления события синхронно с сущностью. Она не является межdocumентной связью и не участвует в сопоставлении "тот же курс лечения" между разными документами — межdocumентная связность намеренно не хранится нигде в модели (см. `../architecture/02-processing-pipeline.md` §6 "Почему нет постоянных междокументных связей").
- `TimelineEvent` не редактируется вручную — пересобирается автоматически при импорте или удалении документа (см. `../mcp/06-timeline.md` §11).
- Удаление `MedicalDocument` удаляет все `TimelineEvent` с этим `documentId`.
- Каждая доменная сущность, имеющая дату (Medication, Diagnosis, Procedure, LabResult, VitalSign), при создании порождает хотя бы одно `TimelineEvent` (см. `../architecture/02-processing-pipeline.md` §7).

---

# 5. Repository

`TimelineRepository`

```text
Add(event TimelineEvent) error
List(userId string, filter TimelineFilter) ([]TimelineEvent, error)
RemoveByDocument(documentId string) error
```

`TimelineFilter` — Value Object, объединяющий `from`/`to`/`types`/`limit` (соответствует параметрам `medical.timeline`, см. `../mcp/06-timeline.md` §4).

---

# 6. Domain Service

`TimelineBuilder` — строит `TimelineEvent` из доменных сущностей одного документа сразу после Normalization (см. `02-processing-pipeline.md` §7). Не использует LLM, не обращается к другим документам пользователя.
