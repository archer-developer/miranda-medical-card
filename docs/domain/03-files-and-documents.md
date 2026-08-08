# File, MedicalDocument, Extraction

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
> - ../architecture/06-storage.md
> - ../mcp/02-files.md
> - ../mcp/03-documents.md

---

# 1. Назначение

Эти три сущности образуют первые три уровня хранения из `../architecture/06-storage.md` §3-4: `File` — оригинальный бинарный объект, `MedicalDocument` — документ как логическая сущность, `Extraction` — сырой результат работы LLM над этим документом. Каждый следующий уровень зависит от предыдущего, но не наоборот.

```text
File ──1:1── MedicalDocument ──1:N── Extraction
```

`MedicalDocument` может иметь несколько версий `Extraction` (см. §4 "Версионирование"), но только одна считается активной.

---

# 2. File

Бинарный объект без медицинской семантики (см. `../mcp/02-files.md`).

## Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`file_...`) | ✅ | Идентификатор файла. |
| `userId` | string | ✅ | Владелец. |
| `filename` | string | ✅ | Оригинальное имя файла на момент загрузки. |
| `contentType` | string | ✅ | MIME Type. |
| `size` | int | ✅ | Размер в байтах. |
| `sha256` | string | ✅ | Хэш содержимого, используется для дедупликации в пределах `(userId, sha256)` — см. `../mcp/02-files.md` §5. |
| `storagePath` | string | ✅ | Путь к файлу в файловом хранилище (никогда не возвращается через MCP). |
| `uploadedAt` | timestamp | ✅ | Момент загрузки. |

## Инварианты

- Неизменяем после создания (§6.6 `../architecture/01-overview.md` "Оригинальные документы никогда не изменяются").
- `(userId, sha256)` — не строгий уникальный ключ, а ключ дедупликации: реализация вправе вернуть существующий `File` вместо создания нового при совпадении.
- Удаляется только как побочный эффект удаления последнего `MedicalDocument`, который на него ссылается (см. `../mcp/02-files.md` §7).

## Repository

`FileRepository`: `Add`, `Get(id, userId)`, `FindBySHA256(userId, sha256)`, `Remove(id, userId)`.

---

# 3. MedicalDocument

Медицинский документ как логическая сущность — см. `../mcp/03-documents.md`.

## Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`doc_...`) | ✅ | Идентификатор документа. |
| `userId` | string | ✅ | Владелец. |
| `fileId` | string | ✅ | Ссылка на `File`. |
| `status` | `DocumentStatus` | ✅ | `PENDING` \| `RUNNING` \| `READY` \| `FAILED` — общий статус прохождения Pipeline, см. `../architecture/02-processing-pipeline.md` §14. |
| `documentType` | `DocumentType` (VO) | ❌ | Определяется Pipeline автоматически; пусто, пока не завершён этап Structured Extraction. |
| `documentDate` | date | ❌ | Дата медицинского события/документа (не дата загрузки). |
| `title` | string | ❌ | Короткое человекочитаемое название, для списков. |
| `organization` | string | ❌ | Клиника/лаборатория, выдавшая документ. |
| `doctor` | string | ❌ | Врач, указанный в документе. |
| `recognizedText` | text | ❌ | Полный текст, распознанный OCR/Vision. Внутреннее поле — не возвращается через MCP (см. `../mcp/03-documents.md` §7). |
| `summary` | text | ❌ | Краткое описание документа, см. §9 `../architecture/02-processing-pipeline.md`. Только факты, без выводов модели. |
| `uploadedAt` | timestamp | ✅ | Момент импорта (`upload_document`). |

## Инварианты

- Не редактируется вручную после успешного импорта — любое изменение происходит только через повторный запуск Pipeline (см. `../architecture/02-processing-pipeline.md` §13), а это административная CLI-операция, не MCP.
- `status` монотонно движется `PENDING → RUNNING → (READY | FAILED)`; повторный запуск возвращает документ в `RUNNING`.
- Удаление `MedicalDocument` каскадно удаляет все зависящие производные сущности в порядке, заданном `../architecture/06-storage.md` §9 (Timeline → Domain Entities → Extraction → Summary → Embeddings → FTS → File).

## Repository

`DocumentRepository`: `Add`, `Get(id, userId)`, `List(userId, filter)`, `UpdateStatus`, `Remove(id, userId)`.

---

# 4. Extraction

"Сырой" JSON, извлечённый LLM на этапе Structured Extraction (см. `../architecture/02-processing-pipeline.md` §5). Является промежуточным представлением: не используется напрямую при поиске или ответах, нужен для повторной Normalization и отладки качества извлечения.

## Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`extr_...`) | ✅ | Идентификатор версии Extraction. |
| `documentId` | string | ✅ | Ссылка на `MedicalDocument`. |
| `version` | int | ✅ | Номер версии (растёт при каждом повторном запуске Extraction — новым prompt/моделью). |
| `active` | bool | ✅ | Только одна версия на документ может быть `active == true`; именно она используется для Normalization. |
| `promptVersion` | string | ❌ | Версия промпта, которым получена эта Extraction — для воспроизводимости и сравнения качества (см. `../architecture/05-llm.md` §15-16). |
| `modelUsed` | string | ❌ | Провайдер/модель (например `gemini-3.5-pro`). |
| `raw` | JSON | ✅ | Сырой результат: `documentType`, `documentDate`, `organization`, `doctor`, `diagnoses[]`, `medications[]`, `labResults[]`, `procedures[]`, `recommendations[]` — см. пример в `../architecture/02-processing-pipeline.md` §5. |
| `createdAt` | timestamp | ✅ | Момент создания этой версии. |

## Инварианты

- Неизменяема после создания — новая попытка извлечения создаёт новую версию, а не модифицирует существующую.
- Ровно одна `active` версия на документ в любой момент времени; активация новой версии деактивирует предыдущую в той же транзакции.
- Normalization (построение Medication/Diagnosis/Procedure/LabResult/TimelineEvent) всегда читает только `active`-версию.

## Repository

`ExtractionRepository`: `Add`, `ListVersions(documentId)`, `GetActive(documentId)`, `Activate(id)`.

---

# 5. Пример жизненного цикла

```text
upload_file()          → File{status: n/a}
upload_document(fileId) → MedicalDocument{status: PENDING}
                        → MedicalDocument{status: RUNNING}
OCR/Vision              (заполняет recognizedText)
Structured Extraction   → Extraction{version: 1, active: true}
Normalization            (строит Medication/Diagnosis/... — см. 06-medication.md и далее)
Timeline                 (строит TimelineEvent — см. 04-timeline.md)
Summary                  (заполняет MedicalDocument.summary)
                        → MedicalDocument{status: READY}
```

Повторная обработка с новым prompt:

```text
NEW PROMPT → Extraction{version: 2, active: true}
           → Extraction{version: 1, active: false}
           → Normalization перестраивает Medication/Diagnosis/... для этого documentId заново
             (полная замена, без сопоставления отдельных старых и новых сущностей —
              см. ../architecture/02-processing-pipeline.md §6 "Идемпотентность Normalization")
```
