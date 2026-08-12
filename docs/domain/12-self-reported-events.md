# SelfReportedEvent, MedicationIntake

> **Статус:** Draft
>
> **Раздел:** Domain
>
> **Версия:** 1.0
>
> **Связанные документы**
>
> - 01-overview.md
> - 03-files-and-documents.md
> - 04-timeline.md
> - 06-medication.md
> - ../architecture/02-processing-pipeline.md
> - ../mcp/07-events.md

---

# 1. Назначение

Обе сущности представляют медицинские факты, зафиксированные пользователем напрямую через диалог с Miranda — без исходного документа. Например:

> Приступ головной боли, принял 400 мг ибупрофена.

> Давление поднялось выше обычного, принял 20 мг моксонидина.

Это второй, параллельный `MedicalDocument`, вид первичного источника данных: там, где `MedicalDocument` порождается из `File`, `SelfReportedEvent` порождается из текста, введённого пользователем. Принцип "оригинал неизменен, всё остальное — производное" (`../architecture/01-overview.md` §6) от этого не нарушается — просто у сервиса теперь два вида неизменяемого оригинала, а не один.

---

# 2. Почему не переиспользован MedicalDocument

`MedicalDocument` неразрывно завязан на `File` (`fileId` обязателен, см. `03-files-and-documents.md` §3) и на этапы Pipeline, специфичные для бинарных файлов — OCR/Vision, дедупликация по SHA256. Ни то, ни другое не имеет смысла для короткой текстовой записи. Поэтому вместо расширения `MedicalDocument` необязательными полями введена отдельная сущность с собственным, более лёгким вариантом Pipeline (см. §5).

---

# 3. SelfReportedEvent

## Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`selfevt_...`) | ✅ | Идентификатор. |
| `userId` | string | ✅ | Владелец. |
| `rawText` | text | ✅ | Исходный текст пользователя, дословно. Неизменяем после создания — играет роль `File` для этой сущности (см. §2). |
| `occurredAt` | datetime | ✅ | Момент, когда событие произошло по словам пользователя. Если не указан явно, по умолчанию равен моменту логирования. |
| `loggedAt` | timestamp | ✅ | Момент фактической записи (`medical.log_event`). Может заметно отличаться от `occurredAt` — пользователь вправе задним числом сообщить "вчера болела голова". |
| `status` | `DocumentStatus` | ✅ | `PENDING` \| `RUNNING` \| `READY` \| `FAILED` — тот же словарь статусов, что у `MedicalDocument` (см. `03-files-and-documents.md` §3), проходит через тот же по духу, но более короткий Pipeline. |
| `category` | enum (`symptom`, `observation`, `medication_intake`, `other`) | ❌ | Результат Structured Extraction — во что в первую очередь классифицируется событие. Заполняется Pipeline, не пользователем. |
| `description` | text | ❌ | Краткое структурированное описание (аналог `MedicalDocument.summary`) — только факты, без интерпретации. |
| `medicationIntakeId` | string | ❌ | Ссылка на `MedicationIntake`, если из текста распознан факт приёма лекарства (см. §4). |

## Инварианты

- `rawText` неизменяем после создания. Исправление ошибки — это создание нового события и удаление старого (`medical.delete_event`), а не редактирование.
- Если Structured Extraction не смогла распознать ничего структурированного (`category`/`description` остались пустыми), событие всё равно сохраняется со `status: READY` — сырой текст пользователя не теряется только потому, что автоматическая структуризация не удалась (в отличие от `MedicalDocument`, где `PIPELINE_FAILED` — обычная ошибка, см. `../mcp/03-documents.md` §4; здесь это осознанно другое поведение, см. `../mcp/07-events.md` §5).

---

# 4. MedicationIntake

Представляет факт однократного приёма лекарства — **не** то же самое, что `Medication` (см. `06-medication.md`), который моделирует курс лечения со статусом `active`/`discontinued`/`completed`. `MedicationIntake` — это мгновенное событие без начала/конца и без статуса.

## Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`intake_...`) | ✅ | Идентификатор. |
| `userId` | string | ✅ | Владелец. |
| `sourceType` | enum (`self_reported`, `document`) | ✅ | Откуда взят факт приёма. На практике сейчас — почти всегда `self_reported`; `document` оставлен для случая, когда однократный приём явно упомянут в документе (например, введение препарата во время госпитализации), а не в виде курса. |
| `sourceId` | string | ✅ | `SelfReportedEvent.id` или `MedicalDocument.id`, в зависимости от `sourceType`. |
| `drugName` | string | ✅ | Канонизированное название действующего вещества — та же нормализация, что и у `Medication.drugName` (см. `06-medication.md` §2). |
| `dose` | `Dosage` (VO, см. `11-value-objects.md`) | ❌ | Доза, если указана. |
| `takenAt` | datetime | ✅ | Момент приёма. |
| `reason` | string | ❌ | Повод (например "головная боль"), если указан. |

## Инварианты

- Не имеет статуса и не участвует в `MedicalProfile.activeMedications` — это разовый факт, а не назначение (см. `05-medical-profile.md`). Смешивание `MedicationIntake` с `Medication` в одной агрегации сделало бы "текущие лекарства" неотличимыми от "что-то разово выпил месяц назад".
- `drugName` канонизируется той же логикой, что и `Medication.drugName`, чтобы `medical.ask` мог отвечать на вопросы вида "сколько раз я принимал ибупрофен за последний месяц", объединяя факты из `MedicationIntake` независимо от того, как именно пользователь назвал препарат в тексте.

---

# 5. Timeline

Оба типа событий порождают `TimelineEvent` (см. `04-timeline.md` §2, поле `sourceEntityType` расширено значениями `self_reported_event`, `medication_intake`):

| Что произошло | `TimelineEvent.type` | `sourceEntityType` |
|---|---|---|
| Зафиксирован симптом/наблюдение | `symptom` | `self_reported_event` |
| Зафиксирован приём лекарства | `medication_taken` | `medication_intake` |

Одна запись `medical.log_event` может породить оба события сразу (симптом + связанный приём лекарства), как в примере из §1 — `TimelineEvent.documentId` в обоих случаях отсутствует (`null`); ссылка идёт только через `sourceEntityType`/`sourceEntityId`, поскольку документа не существует.

---

# 6. Достоверность источника

В отличие от данных, извлечённых из документа (лабораторный анализ, назначение врача — верифицируемые записи третьей стороны), `SelfReportedEvent`/`MedicationIntake` — это неверифицируемое утверждение пользователя. Это не повод исключать их из ответов `medical.ask`, но повод не смешивать их с документальными фактами без разметки источника:

- в `sources` ответа `medical.ask` (см. `../mcp/04-medical.md` §5) такая ссылка не содержит `documentId`, только `eventId`;
- модель (Agent Loop, см. `../architecture/05-llm.md` §3) должна формулировать ответ так, чтобы было понятно происхождение факта (например "по вашим собственным записям..." вместо "согласно результатам анализа...") — см. `../mcp/04-medical.md` §15;
- при ранжировании (`../architecture/04-search.md` §15, §17) `SelfReportedEvent`/`MedicationIntake` — структурированные данные в SQLite и поэтому по-прежнему приоритетнее FTS/Embeddings, но не наравне с документально подтверждёнными фактами. См. `../architecture/04-search.md` §17 за конкретной шкалой Confidence.

Свободный текст `SelfReportedEvent.rawText`/`description` — хороший кандидат для Embedding Search: вопросы вида "когда мне было плохо" (см. `../architecture/04-search.md` §13) чаще относятся именно к самостоятельным записям пользователя, чем к формальным документам.

---

# 7. Repository

`SelfReportedEventRepository`

```text
Add(e SelfReportedEvent) error
Get(id, userId string) (SelfReportedEvent, error)
UpdateStatus(id string, status DocumentStatus) error
Remove(id, userId string) error // каскадно удаляет связанный MedicationIntake и TimelineEvent-ы
```

`MedicationIntakeRepository`

```text
Add(i MedicationIntake) error
ListByUser(userId string, filter DateRange) ([]MedicationIntake, error)
RemoveBySource(sourceType string, sourceId string) error
```

---

# 8. Repository и удаление

Удаление `SelfReportedEvent` следует тому же порядку сверху вниз, что и удаление `MedicalDocument` (см. `../architecture/06-storage.md` §9), только короче — нет `File`, который нужно было бы освобождать:

```text
Delete SelfReportedEvent
  ↓
Delete TimelineEvent (по sourceEntityId)
  ↓
Delete MedicationIntake (если есть)
  ↓
Delete Embeddings / FTS
```
