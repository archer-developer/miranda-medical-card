# Medication

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
> - 05-medical-profile.md
> - 11-value-objects.md

---

# 1. Назначение

Представляет одно назначение лекарственного препарата, извлечённое из одного документа. Источник знаний о текущих и прошлых назначениях.

---

# 2. Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`med_...`) | ✅ | Идентификатор. |
| `userId` | string | ✅ | Владелец. |
| `documentId` | string | ✅ | Документ-источник. |
| `drugName` | string | ✅ | Канонизированное название действующего вещества (например `Rosuvastatin`) — см. `../architecture/02-processing-pipeline.md` §6 "канонизируются названия". |
| `tradeName` | string | ❌ | Торговое название, как указано в документе, если отличается от `drugName`. |
| `dose` | `Dosage` (VO) | ❌ | Дозировка. |
| `frequency` | string | ❌ | Схема приёма в свободной форме (например "1 раз в день") — не структурируется дальше без явной необходимости. |
| `route` | string | ❌ | Способ приёма (`oral`, `injection`, `topical`, ...), если указан в документе. |
| `startedAt` | date | ❌ | Дата начала приёма. |
| `endedAt` | date | ❌ | Дата отмены/завершения. `null`, пока препарат считается активным. |
| `status` | `MedicationStatus` (VO) | ✅ | `active` \| `discontinued` \| `completed`. |
| `reason` | string | ❌ | Причина назначения, если указана. |
| `prescribedBy` | string | ❌ | Врач, назначивший препарат. |

---

# 3. Определение `status`

`status` устанавливается Normalization на основе того, что явно сказано **в этом документе** — не путём сопоставления с другими документами (см. `../architecture/02-processing-pipeline.md` §6 "Почему нет постоянных междокументных связей"):

- документ явно говорит "назначен"/"начат" → `active`, `endedAt = null`;
- документ явно говорит "отменён"/"завершён курс" → `discontinued`/`completed`, `endedAt` = дата из документа.

Определение **текущего** статуса приёма конкретного препарата на уровне всей истории пользователя (например, был ли более поздний документ, отменивший его) — задача `Medication Resolver` при пересборке `MedicalProfile`, а не поле, хранимое на самой сущности `Medication` (см. `05-medical-profile.md` §3 и `04-timeline.md` §4).

---

# 4. Timeline

Medication порождает как минимум одно `TimelineEvent`:

| `Medication.status` (при создании) | `TimelineEvent.type` |
|---|---|
| `active` (впервые встречен `startedAt`) | `medication_started` |
| `discontinued`/`completed` | `medication_stopped` |

Изменение дозировки в рамках того же документа — `medication_changed`.

---

# 5. Инварианты

- Принадлежит ровно одному документу; повторная обработка документа удаляет и пересоздаёт все его `Medication` целиком (документ-скоуп replace, см. `03-files-and-documents.md` §4).
- `endedAt`, если задан, не раньше `startedAt`.
- `drugName` обязателен даже если `tradeName` отсутствует — Extraction должен уметь определить хотя бы одно из двух названий, иначе факт не считается извлечённым.

---

# 6. Repository

`MedicationRepository`

```text
Add(m Medication) error
ListByUser(userId string, filter MedicationFilter) ([]Medication, error)
ListByDocument(documentId string) ([]Medication, error)
ReplaceForDocument(documentId string, meds []Medication) error // документ-скоуп replace
```
