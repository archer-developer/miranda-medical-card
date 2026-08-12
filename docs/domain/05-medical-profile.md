# MedicalProfile

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
> - ../mcp/05-profile.md

---

# 1. Назначение

`MedicalProfile` — агрегированное "текущее состояние" здоровья пользователя. Один `MedicalProfile` на пользователя (не история, а снимок). Полностью производная сущность — источник истины остаются структурированные Domain Entities (Medication, Diagnosis, ...), из которых Profile строится.

---

# 2. Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `userId` | string | ✅ | Первичный ключ — один профиль на пользователя. |
| `activeDiagnoses` | `DiagnosisSummary[]` | ✅ | Только диагнозы со статусом `active` или `chronic` (§4 Diagnosis). |
| `chronicConditions` | `DiagnosisSummary[]` | ✅ | Подмножество `activeDiagnoses` со статусом `chronic` — выделено отдельно для удобства UI, не самостоятельный источник данных. |
| `activeMedications` | `MedicationSummary[]` | ✅ | Только `Medication` со статусом `active` (§ Medication `MedicationStatus`). |
| `allergies` | `AllergySummary[]` | ✅ | Все известные аллергии — не имеют статуса "неактивна", если явно не отменены в более позднем документе. |
| `vaccinations` | `ProcedureSummary[]` | ✅ | Подмножество `Procedure` с `type == vaccination`. |
| `latestLabResults` | `LabResultSummary[]` | ✅ | По одному последнему значению на каждый наблюдаемый показатель (см. §3). |
| `latestVitalSigns` | `VitalSignSummary[]` | ✅ | По одному последнему значению на каждый тип показателя. |
| `rebuiltAt` | timestamp | ✅ | Момент последней пересборки. |

`*Summary` — облегчённые проекции соответствующих доменных сущностей (без `userId`/`documentId`, поскольку контекст уже задан профилем), поля соответствуют примерам в `../mcp/05-profile.md` §6-10.

---

# 3. Правило "последнее значение"

Для `latestLabResults`/`latestVitalSigns` "последний" означает: среди всех `LabResult`/`VitalSign` пользователя с данным `indicatorName`/`type`, сущность с максимальной `takenAt`/`measuredAt`. Это чистая агрегация по существующим данным — вычисляется заново при каждой пересборке, ничего не кэшируется как отдельная связь (см. `../architecture/02-processing-pipeline.md` §6).

---

# 4. Инварианты

- Не редактируется вручную ни через MCP, ни через CLI — единственный писатель `ProfileBuilder`.
- Пересобирается **полностью** (не инкрементально) после каждого успешного `upload_document` и `reprocess_document` для этого пользователя — см. `../architecture/02-processing-pipeline.md` §8.
- Не хранит историю изменений — только текущий снимок. История доступна через `Timeline` и `medical.ask`.
- `MedicalProfile.userId` без соответствующей записи означает "профиль ещё не построен" (например, у пользователя нет ни одного `READY`-документа) — в этом случае `medical.profile` возвращает пустые массивы по всем полям, а не ошибку.

---

# 5. Repository

`ProfileRepository`

```text
Get(userId string) (MedicalProfile, error)
Replace(profile MedicalProfile) error   // атомарная полная замена
```

Никаких частичных `Update*` методов — соответствует принципу "пересобирается полностью".

---

# 6. Domain Service

`ProfileBuilder` — вызывается Application Layer после `TimelineBuilder`, читает актуальные Medication/Diagnosis/Procedure/LabResult/VitalSign/Allergy пользователя из соответствующих Repository и строит новый `MedicalProfile` целиком (см. `01-overview.md` §7). Не использует LLM.
