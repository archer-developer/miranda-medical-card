# Procedure

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

---

# 1. Назначение

Представляет медицинскую процедуру: операцию, обследование, госпитализацию, вакцинацию или консультацию, извлечённую из документа.

---

# 2. Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`proc_...`) | ✅ | Идентификатор. |
| `userId` | string | ✅ | Владелец. |
| `documentId` | string | ✅ | Документ-источник. |
| `type` | enum (`surgery`, `examination`, `hospitalization`, `vaccination`, `consultation`, `other`) | ✅ | Категория процедуры. Соответствует подмножеству `TimelineEventType` (см. `11-value-objects.md`). |
| `name` | string | ✅ | Название/описание процедуры как в документе (например "МРТ коленного сустава"). |
| `performedAt` | date | ✅ | Дата проведения. |
| `performedBy` | string | ❌ | Врач/клиника. |
| `notes` | text | ❌ | Заключение или дополнительный контекст. |

---

# 3. Инварианты

- Принадлежит ровно одному документу; повторная обработка документа полностью заменяет набор `Procedure` для этого `documentId`.
- `type == vaccination` — единственное подмножество, читаемое отдельно в `MedicalProfile.vaccinations` (см. `05-medical-profile.md` §2).

---

# 4. Timeline

`Procedure.type` отображается на `TimelineEvent.type` напрямую:

| `Procedure.type` | `TimelineEvent.type` |
|---|---|
| `surgery` | `surgery` |
| `hospitalization` | `hospitalization` |
| `vaccination` | `vaccination` |
| `consultation` | `consultation` |
| `examination`, `other` | `procedure` |

---

# 5. Repository

`ProcedureRepository` — та же структура методов, что у `MedicationRepository` (см. `06-medication.md` §6), плюс:

```text
ListVaccinations(userId string) ([]Procedure, error) // используется ProfileBuilder
```
