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

Не путать с `MedicationIntake` (см. `12-self-reported-events.md` §4) — та сущность представляет разовый, самостоятельно зафиксированный приём (например "выпил ибупрофен от головной боли"), без статуса и без курса лечения. `Medication` — это назначение врача с началом, концом и статусом; `MedicationIntake` — мгновенный факт.

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
| `confirmedEndedAt` | timestamp | ❌ | Момент, когда пользователь сам подтвердил в диалоге, что курс завершён (`medical.complete_medication`, см. §3.1). `null`, пока это не сделано ни разу. |

---

# 3. Определение `status`

`status` устанавливается Normalization на основе того, что явно сказано **в этом документе** — не путём сопоставления с другими документами (см. `../architecture/02-processing-pipeline.md` §6 "Почему нет постоянных междокументных связей"):

- документ явно говорит "назначен"/"начат" → `active`, `endedAt = null`;
- документ явно говорит "отменён"/"завершён курс" → `discontinued`/`completed`, `endedAt` = дата из документа.

Определение **текущего** статуса приёма конкретного препарата на уровне всей истории пользователя (например, был ли более поздний документ, отменивший его) — задача `Medication Resolver` при пересборке `MedicalProfile`, а не поле, хранимое на самой сущности `Medication` (см. `05-medical-profile.md` §3 и `04-timeline.md` §4).

## 3.1. Подтверждение пользователем (`medical.complete_medication`)

В отличие от `Diagnosis`/`PlannedAction`, у `Medication` долгое время не было ручного пути изменения статуса — только Extraction. `medical.complete_medication` (`../mcp/10-medications.md`) — единственное исключение: пользователь может напрямую подтвердить, что курс завершён ("я закончил принимать курс антибиотиков"), не дожидаясь нового документа, который бы это подтвердил. Кандидатами выступают только записи из текущего (`Medication Resolver`-агрегированного) набора активных препаратов — устаревшая `active`-запись, которую более поздний документ уже отменил, никогда не предлагается.

При подтверждении `status` становится обычным `completed` — отдельного статуса для "пользователь сам завершил" не вводится. Источник записывается отдельно, в `confirmedEndedAt`:

- `confirmedEndedAt == null` — `status`/`endedAt` отражают то, что сказал документ (Extraction);
- `confirmedEndedAt` задан — пользователь подтвердил это сам в диалоге, независимо от того, что говорит документ.

`confirmedEndedAt` защищает запись при повторной обработке исходного документа — см. §5.

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

- Принадлежит ровно одному документу; повторная обработка документа удаляет и пересоздаёт его `Medication`-записи, **кроме** тех, что пользователь уже подтвердил завершёнными (`confirmedEndedAt` задан, см. §3.1) — та запись остаётся нетронутой, а свежая экстракция вставляется отдельной строкой, без попытки согласования между ними (тот же принцип, что и у `Diagnosis`/`PlannedAction`, см. `07-diagnosis-and-allergy.md` §5, `14-planned-action.md` §4).
- `endedAt`, если задан, не раньше `startedAt`.
- `drugName` обязателен даже если `tradeName` отсутствует — Extraction должен уметь определить хотя бы одно из двух названий, иначе факт не считается извлечённым.

---

# 6. Repository

`MedicationRepository`

```text
Add(m Medication) error
ListByUser(userId string, filter MedicationFilter) ([]Medication, error)
ListByDocument(documentId string) ([]Medication, error)
ReplaceForDocument(documentId string, meds []Medication) error // документ-скоуп replace, сохраняет confirmedEndedAt-записи
MarkEndedManually(id string, userId string, at timestamp) error // medical.complete_medication: status="completed", endedAt=at, confirmedEndedAt=at
```
