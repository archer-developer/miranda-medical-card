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
| `activeDiagnoses` | `DiagnosisSummary[]` | ✅ | Только диагнозы со статусом `active` или `chronic` (§4 Diagnosis). Каждая запись несёт `overdue` — вычисленный на чтении флаг "ожидаемый срок разрешения уже прошёл" (`Diagnosis.Overdue`, см. `07-diagnosis-and-allergy.md`), не влияющий на сам статус. |
| `chronicConditions` | `DiagnosisSummary[]` | ✅ | Подмножество `activeDiagnoses` со статусом `chronic` — выделено отдельно для удобства UI, не самостоятельный источник данных. |
| `activeMedications` | `MedicationSummary[]` | ✅ | Только `Medication` со статусом `active` (§ Medication `MedicationStatus`). |
| `allergies` | `AllergySummary[]` | ✅ | Все известные аллергии — не имеют статуса "неактивна", если явно не отменены в более позднем документе. |
| `vaccinations` | `ProcedureSummary[]` | ✅ | Подмножество `Procedure` с `type == vaccination`. |
| `surgeries` | `ProcedureSummary[]` | ✅ | Подмножество `Procedure` с `type == surgery` — как и `vaccinations`, без ограничения по давности: перенесённая операция (например холецистэктомия) остаётся частью профиля независимо от того, сколько лет прошло. |
| `latestLabResults` | `LabResultSummary[]` | ✅ | По одному последнему значению на каждый наблюдаемый показатель (см. §3). |
| `latestVitalSigns` | `VitalSignSummary[]` | ✅ | По одному последнему значению на каждый тип показателя. |
| `nutritionGuidance` | `NutritionGuidance` | ✅ | Диетические ограничения и рекомендации (`restrictions`/`recommendations`, каждый пункт — `text`+`reason`), выведенные из активных диагнозов/хронических состояний, аллергий, активных лекарств, перенесённых операций, возраста/пола и симптомов пользователя за последний месяц. См. §6 и `../adr/006-nutrition-guidance.md`. Оба списка пусты, если нет ни одного диагноза/аллергии/лекарства/операции/симптома, из которого можно было бы что-то вывести. |
| `rebuiltAt` | timestamp | ✅ | Момент последней пересборки. |

`*Summary` — облегчённые проекции соответствующих доменных сущностей (без `userId`/`documentId`, поскольку контекст уже задан профилем), поля соответствуют примерам в `../mcp/05-profile.md` §6-10.

---

# 3. Правило "последнее значение"

Для `latestLabResults`/`latestVitalSigns` "последний" означает: среди всех `LabResult`/`VitalSign` пользователя с данным `indicatorName`/`type`, сущность с максимальной `takenAt`/`measuredAt`. Это чистая агрегация по существующим данным — вычисляется заново при каждой пересборке, ничего не кэшируется как отдельная связь (см. `../architecture/02-processing-pipeline.md` §6).

---

# 4. Инварианты

- Не редактируется вручную ни через MCP, ни через CLI — единственный писатель `ProfileBuilder`.
- Пересобирается **полностью** (не инкрементально) после каждого успешного `upload_document` и `reprocess_document` для этого пользователя — см. `../architecture/02-processing-pipeline.md` §8 — а также после `medical.log_event`/`medical.delete_event`, поскольку `nutritionGuidance` (§6) зависит и от симптомов, зафиксированных напрямую в диалоге, не только из документов.
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

`ProfileBuilder` — вызывается Application Layer после `TimelineBuilder`, читает актуальные Medication/Diagnosis/Procedure/LabResult/VitalSign/Allergy пользователя из соответствующих Repository и строит новый `MedicalProfile` целиком (см. `01-overview.md` §7). Не использует LLM ни для одного поля, кроме `nutritionGuidance`.

`nutritionGuidance` — единственное поле, для которого агрегации недостаточно: сопоставление диагноза
("холецистэктомия") со строгим ограничением жирной пищи, или симптома ("запоры") с рекомендацией по
клетчатке, — открытое медицинское рассуждение над свободным текстом, а не группировка/фильтрация уже
структурированных данных. Оно строится отдельным Domain Service — **Nutrition Advisor**
(`internal/nutrition`, по аналогии с уже описанными Medication/Diagnosis Resolver выше) — одним
Structured LLM-вызовом (переиспользует `llm.extraction_provider`, отдельного провайдера под это не
заведено). Nutrition Advisor вызывается не из `ProfileBuilder` (который остаётся `Без LLM` для всех
остальных полей), а отдельным шагом внутри `Pipeline.rebuildProfile`, после того как `ProfileBuilder`
уже построил остальной `MedicalProfile` — подробности и рассмотренные альтернативы см. в
`../adr/006-nutrition-guidance.md`.
