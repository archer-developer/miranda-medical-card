# Value Objects

> **Статус:** Draft
>
> **Раздел:** Domain
>
> **Версия:** 1.0
>
> **Связанные документы**
>
> - 01-overview.md
> - 06-medication.md
> - 07-diagnosis-and-allergy.md
> - 09-lab-result-and-vital-sign.md

---

# 1. Назначение

Value Objects инкапсулируют бизнес-правила, которые примитивный тип (`string`, `float64`) не может выразить самостоятельно: единицы измерения, диапазоны, замкнутые перечисления. Value Object не имеет собственного идентификатора и сравнивается по значению.

---

# 2. Dosage

Дозировка препарата (`Medication.dose`, см. `06-medication.md`).

| Поле | Тип | Описание |
|------|-----|----------|
| `amount` | number | Числовое значение. |
| `unit` | string | Единица (`mg`, `ml`, `IU`, ...). |
| `form` | string (опционально) | Форма выпуска (`tablet`, `injection`, `drops`), если указана. |

Инвариант: `amount > 0`, если объект вообще заполнен (пустой `Dosage` допустим — не всякий документ указывает точную дозу).

---

# 3. LabValue

Значение лабораторного показателя (`LabResult.value`, см. `09-lab-result-and-vital-sign.md`).

| Поле | Тип | Описание |
|------|-----|----------|
| `value` | number | Измеренное значение, как в документе. |
| `unit` | string | Единица измерения, как в документе (`U/L`, `mmol/L`, ...). |
| `normalizedValue` | number (опционально) | `value`, приведённое к каноничной для этого показателя единице — см. `09-lab-result-and-vital-sign.md` §7. Пусто, если единица не распознана как приводимая или для показателя ещё не установлена каноничная единица. |
| `normalizedUnit` | string (опционально) | Каноничная единица, к которой приведено `normalizedValue`. |
| `referenceLow` | number (опционально) | Нижняя граница нормы, если указана в документе. |
| `referenceHigh` | number (опционально) | Верхняя граница нормы, если указана в документе. |
| `flag` | enum (`normal`, `low`, `high`) | Вычисляется относительно `referenceLow`/`referenceHigh`, если оба заданы; иначе не заполняется — сервис не подставляет собственные референсные значения (см. `09-lab-result-and-vital-sign.md` §4). |

`normalizedValue`/`normalizedUnit` — производный, перестраиваемый кэш, не факт наравне с остальными полями; см. `09-lab-result-and-vital-sign.md` §7 за механизмом и оговоркой о пересчёте.

---

# 4. BloodPressure

Артериальное давление (`VitalSign.bloodPressure`, см. `09-lab-result-and-vital-sign.md`).

| Поле | Тип | Описание |
|------|-----|----------|
| `systolic` | int | Систолическое (верхнее) давление. |
| `diastolic` | int | Диастолическое (нижнее) давление. |
| `unit` | string | Всегда `mmHg` в текущей версии — поле оставлено для единообразия с другими VO, а не потому что ожидаются другие единицы. |

Отображается как `"130/82"` в MCP-ответах (см. `../mcp/05-profile.md` §10) — это представление для чтения, а не формат хранения.

---

# 5. DateRange

Период времени. Используется везде, где нужно выразить "от" и, опционально, "до" — например, границы фильтрации Timeline (`from`/`to`, см. `04-timeline.md` §5) или период приёма препарата, если он моделируется как диапазон, а не парой `startedAt`/`endedAt` на самой сущности.

| Поле | Тип | Описание |
|------|-----|----------|
| `from` | date | Начало периода. |
| `to` | date (опционально) | Конец периода. Отсутствует — период считается открытым/продолжающимся. |

Инвариант: если `to` задан, `to >= from`.

---

# 6. MedicationStatus

Замкнутое перечисление (`Medication.status`, см. `06-medication.md` §2-3).

| Значение | Значение |
|---|---|
| `active` | Приём продолжается на момент документа. |
| `discontinued` | Приём отменён врачом до планового завершения курса. |
| `completed` | Курс завершён по плану. |

Не путать с агрегированным "текущий статус приёма" на уровне `MedicalProfile` — см. `06-medication.md` §3.

---

# 7. DocumentStatus

Замкнутое перечисление статусов прохождения Pipeline — используется и `MedicalDocument.status` (см. `03-files-and-documents.md` §3), и `SelfReportedEvent.status` (см. `12-self-reported-events.md` §3), поскольку оба проходят через один и тот же по духу процесс, только разной длины.

```text
PENDING
RUNNING
READY
FAILED
```

См. `../architecture/02-processing-pipeline.md` §14 за полным описанием семантики каждого значения.

---

# 8. DocumentType

Замкнутое (но расширяемое без изменения MCP API — см. `../mcp/03-documents.md` §10) перечисление типов документа (`MedicalDocument.documentType`, см. `03-files-and-documents.md` §3).

Известные значения на текущий момент: `lab_report`, `consultation`, `discharge_summary`, `prescription`, `imaging_report`, `referral`, `other`.

Значение определяется Pipeline автоматически на этапе Structured Extraction — не запрашивается у пользователя (см. `../mcp/03-documents.md` §10).

---

# 9. TimelineEventType

Замкнутое (расширяемое) перечисление типов событий Timeline (`TimelineEvent.type`, см. `04-timeline.md` §2), полный список — `../mcp/06-timeline.md` §6:

```text
consultation
diagnosis
procedure
hospitalization
surgery
medication_started
medication_changed
medication_stopped
lab_result
vaccination
vital_sign
document
symptom
medication_taken
```

Отображения из доменных сущностей в конкретное значение см. в соответствующих файлах: `06-medication.md` §4 (Medication), `07-diagnosis-and-allergy.md` §2/§3 (Diagnosis/Allergy), `08-procedure.md` §4 (Procedure), `09-lab-result-and-vital-sign.md` §2/§3 (LabResult/VitalSign), `12-self-reported-events.md` §5 (SelfReportedEvent/MedicationIntake — единственные два типа без `documentId`).

---

# 10. MedicalCode

Код во внешней системе кодирования. Используется у нескольких сущностей с одинаковой формой: `Diagnosis.code` (см. `07-diagnosis-and-allergy.md` §2) и `LabResult.code` (см. `09-lab-result-and-vital-sign.md` §2) — концептуально один и тот же VO, а не два похожих совпадением.

| Поле | Тип | Описание |
|------|-----|----------|
| `system` | enum (`icd10`, `loinc`, ...) | Система кодирования. Поддерживаемые на текущий момент значения — `icd10` (для `Diagnosis`) и `loinc` (для `LabResult`, см. `09-lab-result-and-vital-sign.md` §8); поле существует для будущего расширения (SNOMED CT и др., см. `../architecture/04-search.md` §24), а не потому что произвольная система уже поддержана. |
| `code` | string | Сам код (например `I10` для ICD-10, `718-7` для LOINC). |

Value Object не имеет собственного текстового описания — человекочитаемое название хранится отдельно на самой сущности (`Diagnosis.name`, `LabResult.indicatorName`), поскольку название нужно всегда, а код — не всегда доступен.

---

# 11. InstrumentalModality

Метод инструментального исследования (`InstrumentalFinding.modality`, см. `13-instrumental-finding.md` §3).

```text
ultrasound
mri
ct
xray
ecg
echo_kg
other
```

Отдельная от `Procedure.type` (см. `08-procedure.md` §2) классификация: `Procedure.type` описывает *категорию события* ("это обследование"), `InstrumentalModality` — *конкретный метод*, которым оно выполнено. Одному `Procedure` с `type: examination` соответствует ровно одна `InstrumentalModality`.

---

# 12. Общий принцип

Ни один Value Object не имеет собственного `id`, собственного Repository или собственного жизненного цикла — он существует только как часть содержащей его Entity и удаляется/пересоздаётся вместе с ней.
