# LabResult, VitalSign

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

Обе сущности представляют измеренное значение показателя на определённую дату — лабораторный анализ и показатель жизнедеятельности соответственно. Сгруппированы вместе из-за идентичной формы (показатель + значение + дата).

---

# 2. LabResult

## Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`lab_...`) | ✅ | Идентификатор. |
| `userId` | string | ✅ | Владелец. |
| `documentId` | string | ✅ | Документ-источник. |
| `indicatorName` | string | ✅ | Название показателя, канонизированное (например `ALT`, `LDL`) — см. `../architecture/02-processing-pipeline.md` §6. |
| `value` | `LabValue` (VO) | ✅ | Значение + единица измерения + референсный диапазон, см. `11-value-objects.md`. |
| `takenAt` | date | ✅ | Дата забора/исследования. |

## Timeline

Порождает `TimelineEvent{type: "lab_result"}`.

---

# 3. VitalSign

## Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`vital_...`) | ✅ | Идентификатор. |
| `userId` | string | ✅ | Владелец. |
| `documentId` | string | ✅ | Документ-источник. |
| `type` | enum (`blood_pressure`, `weight`, `height`, `pulse`, `temperature`) | ✅ | Тип показателя. |
| `bloodPressure` | `BloodPressure` (VO) | ❌ | Заполнено только при `type == blood_pressure`. |
| `value` | number | ❌ | Числовое значение для всех типов, кроме `blood_pressure` (там используется `bloodPressure` — систолическое/диастолическое давление не одно число). |
| `unit` | string | ❌ | Единица измерения (`kg`, `cm`, `bpm`, `°C`). |
| `measuredAt` | date | ✅ | Дата измерения. |

## Timeline

Порождает `TimelineEvent{type: "vital_sign"}`.

---

# 4. Общие инварианты

- Обе сущности принадлежат ровно одному документу; повторная обработка документа полностью заменяет набор `LabResult`/`VitalSign` для этого `documentId`.
- Показатель без числового значения (например "анализ не завершён") не создаёт сущность — Extraction обязан иметь и название показателя, и значение, иначе факт не считается извлечённым.
- `LabValue.referenceRange`, если известен из документа, сохраняется как указано в документе — сервис не подставляет собственные референсные диапазоны в v1 (это отмечено как возможное будущее расширение через LOINC, см. `../architecture/04-search.md` §24).

---

# 5. "Последнее значение" в Medical Profile

`MedicalProfile.latestLabResults`/`latestVitalSigns` выбирают по одной записи на каждый уникальный `indicatorName`/`type` пользователя — правило агрегации описано в `05-medical-profile.md` §3, не является полем самой сущности.

---

# 6. Repository

`LabResultRepository`, `VitalSignRepository` — та же структура методов, что у `MedicationRepository` (см. `06-medication.md` §6), плюс:

```text
LatestByIndicator(userId string) (map[string]LabResult, error)   // используется ProfileBuilder
LatestByType(userId string) (map[string]VitalSign, error)        // используется ProfileBuilder
HistoryByIndicator(userId, indicatorName string) ([]LabResult, error) // используется Lab Provider, см. ../architecture/04-search.md §7
```
