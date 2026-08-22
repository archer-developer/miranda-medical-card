# CLI Development Tools

> **Статус:** Draft
>
> **Раздел:** CLI
>
> **Версия:** 1.0
>
> **Команда:** `medical-dev`

---

# 1. Назначение

`medical-dev` — это набор инструментов для разработки, тестирования и отладки Medical Service.

Команды данного раздела предназначены исключительно для разработчика сервиса и не используются Miranda.

Основная цель — сделать внутреннюю работу сервиса максимально прозрачной и упростить разработку новых возможностей.

---

# 2. Основные принципы

Команды `medical-dev` должны:

- максимально подробно отображать внутреннюю работу сервиса;
- использовать те же Application Services, что и MCP API;
- не обращаться напрямую к Repository или SQLite;
- не дублировать бизнес-логику;
- помогать в диагностике, а не изменять архитектуру приложения.

По умолчанию команды работают в режиме "только чтение". Любые операции, изменяющие данные, должны быть явно указаны пользователем.

---

# 3. Общая структура

```text
medical-dev <command> [options]
```

Например:

```bash
medical-dev ask --user alex "Почему врач отменил статины?"
```

---

# 4. Команда: ask

Выполняет полный цикл обработки вопроса аналогично `medical.ask`, но отображает все внутренние этапы работы.

## Пример

```bash
medical-dev ask \
    --user alex \
    "Почему врач отменил статины?"
```

## Пример вывода

```text
Question
────────────────────────────────────────

Почему врач отменил статины?

Agent Loop
────────────────────────────────────────

Tool calls

 ✓ TimelineProvider
 ✓ MedicationProvider
 ✓ LabProvider
 ✓ DocumentProvider

Providers
────────────────────────────────────────

TimelineProvider

  14 events

MedicationProvider

  3 medications

LabProvider

  ALT
  AST
  LDL

Search
────────────────────────────────────────

Embedding Search

  4 chunks

FTS Search

  6 chunks

Merged Context

  8 chunks

LLM
────────────────────────────────────────

Provider

  Gemini

Model

  gemini-2.5-pro

Input Tokens

  4 812

Output Tokens

  396

Duration

  2.9 s

Answer
────────────────────────────────────────

...
```

---

# 5. Команда: planner

**Устарело**: Planner как отдельный, изолированный от генерации ответа вызов LLM упразднён (см. `../adr/001-internal-agent-loop-implementation.md`, `../architecture/05-llm.md` §3) — выбор Knowledge Providers теперь часть одного открытого диалога Agent Loop, а не отдельное решение, которое можно показать в изоляции от остального ответа. Эта команда никогда не была реализована (см. `cmd/medical-dev/main.go`'s package doc comment) и теперь не будет — раздел сохранён как исторический контекст, не как план.

## Пример

```bash
medical-dev planner \
    --user alex \
    "Что происходило прошлой зимой?"
```

## Пример вывода

```text
Selected Providers

✓ TimelineProvider

✓ MedicationProvider

✓ DocumentProvider

Reason

Temporal question
```

---

# 6. Команда: provider

Позволяет протестировать отдельный Knowledge Provider.

## Пример

```bash
medical-dev provider timeline \
    --user alex \
    --question "Когда впервые повысился ALT?"
```

или

```bash
medical-dev provider medications \
    --user alex
```

Выводит только результат работы выбранного Provider.

---

# 7. Команда: search

Отображает результаты поиска без генерации ответа.

Используется для настройки алгоритмов поиска и оценки качества индексации.

## Пример

```bash
medical-dev search \
    --user alex \
    "статины"
```

## Пример вывода

```text
Embedding Search

Document #12

Score: 0.94

Document #27

Score: 0.91

FTS Search

Document #18

Rank: 27.4

Merged Results

7 chunks
```

---

# 8. Команда: prompt

Формирует итоговый Prompt, который будет отправлен LLM.

Никакой запрос к модели при этом не выполняется.

## Пример

```bash
medical-dev prompt \
    --user alex \
    "Какие лекарства я сейчас принимаю?"
```

Позволяет анализировать:

- выбранный контекст;
- инструкции;
- Knowledge Providers;
- итоговый Prompt.

---

# 9. Команда: profile

Показывает текущий Medical Profile пользователя.

## Пример

```bash
medical-dev profile --user alex
```

Используется при разработке Profile Providers и диагностике агрегированных данных.

---

# 10. Команда: timeline

Показывает Timeline пользователя в читаемом виде.

## Пример

```bash
medical-dev timeline --user alex
```

Дополнительно могут поддерживаться фильтры:

```bash
--from

--to

--type
```

---

# 11. Команда: planned-actions

Показывает Planned Actions пользователя (`../domain/14-planned-action.md`) — будущие медицинские
действия, извлечённые из документов или зафиксированные напрямую в диалоге, с диапазоном срока и
статусом (`pending`/`completed`/`declined`).

## Пример

```bash
medical-dev planned-actions --user alex
```

По умолчанию показывает только `pending` (включая просроченные). Чтобы увидеть также
`completed`/`declined`:

```bash
medical-dev planned-actions --user alex --include-resolved
```

Использует тот же `Pipeline.GetUpcomingPlan`, что и `medical.planned_actions`
(`../mcp/08-planned-actions.md`) — вывод идентичен тому, что получила бы Miranda.

---

# 12. Команда: document

Показывает полную внутреннюю информацию о медицинском документе.

## Пример

```bash
medical-dev document doc_01J8...
```

Вывод может включать:

- метаданные документа;
- распознанный текст;
- Summary;
- извлечённые медицинские сущности;
- Timeline Events;
- Chunks;
- Embeddings;
- связанные объекты.

---

# 13. Команда: pipeline

Запускает Processing Pipeline для уже импортированного документа с подробным логом
выполнения, начиная с этапа, указанного флагом `--stage` (см.
`../architecture/02-processing-pipeline.md` §2 "Независимость этапов" — это
конкретная реализация того принципа для документо-ориентированной части Pipeline):

| `--stage` | Что пропускается | LLM-вызовы | Когда использовать |
|-----------|-------------------|------------|---------------------|
| `ocr` (по умолчанию) | ничего — полный прогон | OCR + Structured Extraction | обычный `medical.reprocess_document` |
| `extraction` | OCR (переиспользуется сохранённый `MedicalDocument.RecognizedText`) | только Structured Extraction | изменилась схема/промпт Structured Extraction (например, `Diagnosis.status`/`expectedResolution`), а распознавание текста переделывать не нужно |
| `normalization` | OCR и Structured Extraction (переигрывается сохранённый `Extraction.Raw` текущего активного Extraction) | ни одного | изменилась только логика Normalization (единицы измерения, разбор дат) — новая версия Extraction при этом не создаётся, см. §2 "Идемпотентность" |

## Пример

```bash
medical-dev pipeline doc_01J8... --user alex
```

## Пример вывода

```text
OCR

✓ completed

Medical Extraction

✓ completed

Timeline

✓ completed

Medical Profile

✓ completed

Embeddings

✓ completed

Finished
```

```bash
medical-dev pipeline doc_01J8... --user alex --stage extraction
medical-dev pipeline doc_01J8... --user alex --stage normalization
```

Флаг `--all` вместо конкретного `documentId` перезапускает выбранный `--stage` для всех
документов пользователя подряд, продолжая при ошибке на отдельном документе (например,
при исчерпании дневной квоты одной конкретной модели — только для `--stage ocr` или
`extraction`, поскольку `normalization` вообще не вызывает LLM) и печатая итоговое
количество неудачных документов:

```bash
medical-dev pipeline --all --user alex --stage extraction
```

Флаг `--provider NAME` подменяет и `llm.ocr_provider`, и `llm.extraction_provider` на
другого сконфигурированного провайдера — тот же приём, что и у `medical-dev backfill-titles` (одноразовая миграция,
не входящая в этот документ — см. её собственный doc comment в `cmd/medical-dev/main.go`),
полезен, когда у провайдера по умолчанию исчерпана дневная квота, но у другого
сконфигурированного провайдера ещё есть запас (не имеет эффекта для `--stage normalization`,
которая не вызывает LLM вовсе):

```bash
medical-dev pipeline --all --user alex --stage extraction --provider gemini-agent
```

Используется при разработке Pipeline и диагностике ошибок обработки документов.

---

# 14. Команда: llm

Проверяет доступность всех LLM-провайдеров.

## Пример

```bash
medical-dev llm
```

## Пример вывода

```text
Gemini

✓ Available

Claude

✓ Available

OpenAI Compatible

✗ Unavailable

Fallback Order

Gemini

↓

Claude

↓

OpenAI Compatible
```

---

# 15. Форматы вывода

Все команды должны поддерживать единый механизм форматирования.

Минимально рекомендуется поддерживать:

| Формат | Назначение |
|---------|------------|
| `text` | Человекочитаемый вывод (по умолчанию) |


---

# 16. Общие параметры

Большинство команд используют одинаковые параметры.

| Параметр | Описание |
|----------|----------|
| `--user` | Идентификатор пользователя |

---

# 17. Архитектурные принципы

Команды `medical-dev` не должны содержать собственной бизнес-логики.

Они являются инструментами диагностики и используют те же Application Services, что и MCP API.

Благодаря этому поведение сервиса при разработке и в рабочем режиме остается идентичным.

---

# 18. Будущие расширения

В дальнейшем раздел `medical-dev` может быть дополнен новыми командами.

Например:

- `medical-dev embeddings`
- `medical-dev providers`
- `medical-dev chunks`
- `medical-dev context`
- `medical-dev ocr`
- `medical-dev benchmark`
- `medical-dev explain`

Все новые команды должны соответствовать общей философии: помогать разработчику понять внутреннюю работу сервиса, не изменяя его архитектуру и публичный MCP API.