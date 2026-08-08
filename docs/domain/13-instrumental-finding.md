# InstrumentalFinding

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
> - 08-procedure.md
> - 09-lab-result-and-vital-sign.md
> - 11-value-objects.md
> - ../architecture/03-knowledge-providers.md
> - ../architecture/04-search.md

---

# 1. Назначение

Представляет один измеренный или описанный параметр, полученный инструментальным методом исследования (УЗИ, МРТ, КТ, рентген, ЭКГ, ЭХО-КГ и т.п.) — размер органа, толщина стенки, эхогенность, проходимость сосуда и подобное.

Параллельна `LabResult` (`09-lab-result-and-vital-sign.md`), но для другого класса медицинских данных. В медицинской практике различают лабораторные и инструментальные методы исследования — модель отражает это разделение напрямую, а не пытается втиснуть оба класса в одну сущность.

---

# 2. Почему не расширили LabResult

`LabResult` предполагает единственный показатель с единственным числовым значением и единицей измерения (`ALT: 28.3 Ед/л`). Инструментальные находки:

- часто описывают конкретную анатомическую структуру ("Печень", "Желчный пузырь", "Почка правая"), а не абстрактный показатель;
- часто не числовые ("эхогенность: обычная", "контуры: ровные") — это тоже клинически значимая информация, которую нужно трендить так же, как числа;
- относятся к конкретному исследованию (`Procedure`), а не существуют сами по себе.

Проще завести отдельную сущность с собственной, слегка отличающейся формой, чем перегружать `LabResult` опциональными полями, нужными только для одного из двух случаев.

---

# 3. Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`finding_...`) | ✅ | Идентификатор. |
| `userId` | string | ✅ | Владелец. |
| `documentId` | string | ✅ | Документ-источник. |
| `procedureId` | string | ❌ | Ссылка на `Procedure`, представляющую само исследование (например "УЗИ органов брюшной полости и почек"). См. §5. |
| `modality` | `InstrumentalModality` (VO, см. `11-value-objects.md`) | ✅ | Метод исследования: `ultrasound` \| `mri` \| `ct` \| `xray` \| `ecg` \| `echo_kg` \| `other`. |
| `structure` | string | ✅ | Анатомическая структура/орган, канонизированная как есть на письме (например "Печень") — та же логика канонизации, что у `Medication.drugName` (см. `06-medication.md` §2). |
| `parameter` | string | ✅ | Измеряемый/описываемый параметр в рамках структуры (например "правая доля КВР", "толщина стенки", "эхогенность", "особенности"). |
| `value` | number | ❌ | Числовое значение, как в документе, если параметр количественный. |
| `unit` | string | ❌ | Единица измерения, как в документе (`мм`, `см`, `см/с`), если `value` задано. |
| `normalizedValue` | number | ❌ | `value`, приведённое к каноничной единице для пары `(structure, parameter)` этого пользователя — тот же механизм, что у `LabResult`, см. `09-lab-result-and-vital-sign.md` §7. Ключ канонизации — `structure + "/" + parameter`, а не только `parameter`, поскольку одно и то же название параметра может встречаться у разных структур с разными единицами. |
| `normalizedUnit` | string | ❌ | Каноничная единица, к которой приведено `normalizedValue`. |
| `qualitativeValue` | string | ❌ | Описательное значение, если параметр не числовой (например "однородная", "не расширена"). Ровно одно из `value`/`qualitativeValue` обычно заполнено, но оба могут быть пустыми, если параметр упомянут без конкретного значения. Единицы измерения не нормализуются, если `unit` пусто. |
| `referenceLow` / `referenceHigh` | number | ❌ | Референсный диапазон, если указан в документе (на практике для инструментальных исследований печатается реже, чем для лабораторных). |
| `measuredAt` | date | ✅ | Дата исследования. |

---

# 4. Инварианты

- Принадлежит ровно одному документу; повторная обработка документа полностью заменяет набор `InstrumentalFinding` для этого `documentId` (документ-скоуп replace, как и у остальных Domain Entities — см. `../architecture/02-processing-pipeline.md` §6).
- `structure` и `parameter` обязательны даже при отсутствии значения — Extraction обязан извлекать хотя бы факт "этот параметр упомянут для этой структуры", а не выдумывать значение, если его нет в тексте.
- Не имеет собственного `status` и не участвует в `MedicalProfile` в текущей версии (см. §7).

---

# 5. Связь с Procedure

Одно инструментальное исследование (`Procedure` с `type: examination`, например "УЗИ органов брюшной полости и почек") обычно порождает много `InstrumentalFinding` — по одной записи на каждый измеренный/описанный параметр.

```text
Procedure (УЗИ брюшной полости, 2026-07-23)
  │
  ├── InstrumentalFinding (Печень, правая доля КВР, 157 мм)
  ├── InstrumentalFinding (Печень, эхогенность, "обычная")
  ├── InstrumentalFinding (Желчный пузырь, толщина стенки, 2.6 мм)
  ├── InstrumentalFinding (Желчный пузырь, особенности, "гиперэхогенные включения...")
  └── ... (остальные структуры/параметры)
```

`procedureId` не обязателен: Extraction может создать `InstrumentalFinding` до того, как Normalization свяжет их с конкретной `Procedure`, либо в случае, когда сам факт исследования не сформулирован в тексте достаточно чётко для создания `Procedure`.

---

# 6. Timeline

`InstrumentalFinding` **не порождает собственных `TimelineEvent`** — иначе одно УЗИ с 20-30 параметрами превратилось бы в 20-30 событий на ленте. Событие уже создаётся на уровне связанной `Procedure` (см. `08-procedure.md` §4). Находки доступны через историю по `(structure, parameter)`, а не через Timeline.

---

# 7. Medical Profile

Сознательно **не включается** в `MedicalProfile` в текущей версии — та же сдержанность, что и в решении не агрегировать `SelfReportedEvent`/`MedicationIntake` (см. `12-self-reported-events.md` §7 и `05-medical-profile.md`). Основной потребитель — `medical.ask` через Instrumental Findings Provider (см. §9), не карточка текущего состояния. Может быть добавлено позже (например "последние размеры ключевых органов"), если появится реальная потребность.

---

# 8. Repository

`InstrumentalFindingRepository`

```text
Add(f InstrumentalFinding) error
ListByDocument(documentId string) ([]InstrumentalFinding, error)
ReplaceForDocument(documentId string, findings []InstrumentalFinding) error
HistoryByStructureParameter(userId, structure, parameter string) ([]InstrumentalFinding, error)
```

`HistoryByStructureParameter` — прямой аналог `LabResultRepository.HistoryByIndicator` (`09-lab-result-and-vital-sign.md` §6), используется Instrumental Findings Provider для ответа на вопросы о динамике (например "как менялся размер печени").

---

# 9. Knowledge Provider

Instrumental Findings Provider регистрируется в Provider Registry наравне с Lab Provider (см. `../architecture/03-knowledge-providers.md` §8). Использует структурированный поиск по SQLite (`HistoryByStructureParameter`), не Embeddings — по тем же причинам, что и Lab Search (см. `../architecture/04-search.md` §7): точный структурный запрос всегда приоритетнее семантического поиска, когда данные уже структурированы.

---

# 10. Пример

Вопрос пользователя:

> Как менялся размер печени по УЗИ за последние два года?

```text
Instrumental Findings Provider
  → HistoryByStructureParameter(userId, "Печень", "правая доля КВР")
  → [
      {value: 157, unit: "мм", measuredAt: "2026-07-23"},
      {value: 149, unit: "мм", measuredAt: "2025-02-10"}
    ]
```

Knowledge Chunk, переданный Answer Generator, формулируется как человекочитаемый факт (см. `../architecture/03-knowledge-providers.md` §12), а не как сырая структура — например:

```
Размер правой доли печени (КВР)

23 июля 2026: 157 мм
10 февраля 2025: 149 мм
```
