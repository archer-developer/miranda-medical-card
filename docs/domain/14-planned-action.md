# PlannedAction

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
> - 12-self-reported-events.md
> - ../architecture/02-processing-pipeline.md
> - ../adr/004-planned-actions.md
> - ../adr/005-planned-action-cross-source-dedup.md
> - ../mcp/08-planned-actions.md

---

# 1. Назначение

Представляет будущее медицинское действие — контрольный анализ, прививку, повторный визит,
обследование — с приблизительным сроком выполнения. Источник — либо рекомендация, найденная в
документе ("Повторный анализ крови через полгода"), либо факт, сообщённый пользователем напрямую
в диалоге с Miranda ("запиши, что мне нужно сделать прививку от бешенства в течение полугода").

Медкарта не имеет собственного механизма напоминаний — эта функция принадлежит Miranda. Смысл
`PlannedAction` — дать Miranda структурированный список того, что предстоит, чтобы она могла
выставить свои собственные напоминания (`medical.planned_actions`, см. `../mcp/08-planned-actions.md`).

Подробное обоснование дизайна (диапазон дат вместо точки, автозавершение вместо ручной отметки,
почему создание/отмена идут через текстовые write-tool'ы, а не через `medical.ask`) — `../adr/004-planned-actions.md`.

---

# 2. Поля

| Поле | Тип | Обязательно | Описание |
|------|-----|-------------|----------|
| `id` | string (`plan_...`) | ✅ | Идентификатор. |
| `userId` | string | ✅ | Владелец. |
| `sourceType` | enum (`document`, `self_reported`) | ✅ | Откуда взят факт — тот же паттерн, что у `MedicationIntake.sourceType` (см. `12-self-reported-events.md` §4). |
| `sourceId` | string | ✅ | `MedicalDocument.id` или `SelfReportedEvent.id`, в зависимости от `sourceType`. |
| `type` | enum (`lab_test`, `surgery`, `examination`, `hospitalization`, `vaccination`, `consultation`, `other`) | ✅ | Категория действия. Совпадает с `Procedure.type` (см. `08-procedure.md` §2) плюс `lab_test` — контрольный анализ закрывается `LabResult`, а не `Procedure`, поэтому у него отдельная категория для целей сопоставления (см. §5). |
| `description` | text | ✅ | Краткое описание, например "Повторный анализ глюкозы крови". |
| `referenceText` | text | ❌ | Исходная формулировка рекомендации, дословно — только для отображения/трассировки, не участвует в сопоставлении. |
| `matchIndicatorName` | string | ❌ | Только `type == lab_test` — канонизированное имя показателя, тем же способом, что и `LabResult.indicatorName` (см. `09-lab-result-and-vital-sign.md` §2). |
| `matchProcedureName` | string | ❌ | Любой другой `type` — канонизированное имя, тем же способом, что и `Procedure.name`. |
| `dueDateFrom` | date | ❌ | Начало диапазона ожидаемого срока. Отсутствует, если в исходном тексте не было указано время ("сдать кровь на глюкозу" без срока) — пункт плана в этом случае не имеет даты и никогда не считается просроченным. |
| `dueDateTo` | date | ❌ | Конец диапазона. |
| `status` | enum (`pending`, `completed`, `declined`) | ✅ | См. §4. |
| `matchedDocumentId` | string | ❌ | Документ, который автоматически закрыл этот пункт (только `status == completed`). |
| `matchedEntityId` | string | ❌ | `LabResult.id` или `Procedure.id`, который его закрыл. |
| `matchedAt` | datetime | ❌ | Момент автозакрытия. |

`dueDateFrom`/`dueDateTo` — диапазон, а не точка: источник почти всегда даёт грубый срок
("через полгода", "в течение месяца"), а не точную дату — см. `../adr/004-planned-actions.md`
за обоснованием этого решения и за тем, почему арифметику дат выполняет детерминированный код
Normalization, а не LLM.

---

# 3. Происхождение

## 3.1. Из документа (`sourceType: document`)

Structured Extraction (`../architecture/02-processing-pipeline.md` §5) распознаёт `plannedActions`
как отдельную структурированную категорию, sibling к `procedures` — не расширение
`Recommendations` (которые остаются как есть, полностью неструктурированным текстом). Модель
извлекает только относительную величину срока (`dueAmountMin`/`dueAmountMax` + `dueUnit`:
day/week/month/year), никогда не вычисляет календарную дату сама. Normalization считает
`dueDateFrom`/`dueDateTo` от `documentDate` (или от момента обработки, если дата документа
неизвестна) — тем же детерминированным способом, что и остальные даты.

## 3.2. Из диалога (`sourceType: self_reported`)

Через уже существующий `medical.log_event` (`../mcp/07-events.md` §3), не через отдельный
write-tool: структурированное извлечение самостоятельно зафиксированных событий
(`internal/events`) дополнительно распознаёт `plannedAction` — независимо от `category`, тем же
способом, что и `medicationIntake` сегодня. Якорь для арифметики дат — момент диалога
(`occurredAt`/момент вызова), а не дата документа, которой здесь нет.

---

# 4. Статус и автозавершение

- `pending` — по умолчанию, при создании.
- `completed` — обычно выставляется **автоматически**: если позже обрабатывается документ, чьи
  нормализованные `LabResult`/`Procedure` совпадают по `type` и канонизированному
  `matchIndicatorName`/`matchProcedureName` с ещё не выполненным пунктом плана, пункт помечается
  `completed` с обратной ссылкой на закрывший его документ/сущность (`internal/planmatch`, см.
  `../architecture/02-processing-pipeline.md`). Ограничение: сопоставление реально работает только
  со стороны Document Pipeline — self-reported факт вроде "я сделал прививку" сам по себе не
  производит `LabResult`/`Procedure` и потому не может автоматически закрыть пункт плана. Для
  этого случая есть ручная альтернатива — `medical.complete_planned_action`
  (`../mcp/08-planned-actions.md` §5): тот же принцип текстового сопоставления, что и у
  `medical.decline_planned_action`, но помечает пункт `completed` вместо `declined`, с `matchedAt`,
  равным моменту подтверждения, и **без** `matchedDocumentId`/`matchedEntityId` — закрывшего
  документа/сущности здесь нет, только слово пользователя.
- `declined` — пользователь отменил пункт плана через диалог (`medical.decline_planned_action`,
  см. `../mcp/08-planned-actions.md`). Терминальный статус: не участвует ни в переопределении
  "просрочено", ни в автозавершении — отменённый пункт не может внезапно "довыполниться" более
  поздним документом.
- "Просрочено" не хранится отдельным полем — вычисляется на чтении: `pending` и `dueDateTo` в
  прошлом. Единственное место, где это считается — `PlannedAction.Overdue` — чтобы MCP tool,
  `medical-dev` и Knowledge Provider не разошлись в определении.

---

# 5. Инварианты

- Пункт с `sourceType: document` принадлежит ровно одному документу, но, в отличие от всех
  остальных document-scoped сущностей, состояние (`status`, `matched*`) должно пережить reprocess
  *другого* документа — поэтому переобработка документа-источника не реплейсит пункты плана вслепую
  (delete+reinsert всех подряд). `ReplaceForSource` удаляет только те существующие пункты этого
  источника, что всё ещё `pending` (дефолтное, ничем не тронутое состояние), и вставляет каждый
  пункт новой Extraction как новую `pending`-запись — `completed`/`declined` запись остаётся как
  есть, ей ничего не соответствует и не заменяет её. Сознательно **не** пытается сопоставить старую
  и новую запись по сильному content-key (`type` + канонизированное имя) — результат Structured
  Extraction не гарантированно одинаков между запусками (см.
  `../architecture/02-processing-pipeline.md` §11), так что этот ключ ненадёжен как идентификатор;
  статус — надёжен. Более простую проверку `ReplaceForSource` всё же делает: если `description`
  новой записи (без учёта регистра/пробелов) точно совпадает с описанием уже выжившей
  (`completed`/`declined`) записи этого же источника, новая не вставляется — `description` так же
  нестабилен между запусками Extraction в принципе, но простая проверка ловит частый случай (текст
  документа не менялся, менялась только сама Extraction) достаточно дёшево, чтобы того стоило. Цена:
  переформулированная (не дословно совпадающая) повторная экстракция того же пункта всё ещё может
  вставиться рядом с уже `completed`-записью как похожий текст того же пункта — редкий, но
  безобидный побочный эффект против тихой потери состояния или хрупкого сопоставления.
- Переобработка документа, который что-то закрыл, безусловно снимает эту связь (сброс в
  `pending`) перед повторным сопоставлением — иначе пункт плана может остаться навсегда
  "выполненным" тем, чего в текущих данных документа уже нет.
- Пункт с `sourceType: self_reported` создаётся и удаляется вместе со своим `SelfReportedEvent`
  (`medical.delete_event` чистит его так же, как уже чистит `MedicationIntake`, см.
  `12-self-reported-events.md` §8) — нет отдельного редактирования, как и у самого
  `SelfReportedEvent.rawText`.
- Пункт без `dueDateFrom`/`dueDateTo` (в тексте не было срока) всегда присутствует в выдаче
  `medical.planned_actions` и никогда не считается просроченным.
- **Дедупликация между разными документами** (`../adr/005-planned-action-cross-source-dedup.md`):
  прежде чем вставить новую pending-запись, `ReplaceForSource` проверяет, нет ли уже у того же
  `userId` pending-записи с тем же ключом (`type` + канонизированное имя) от **другого** источника
  (документа или диалога) — если есть, новая запись не создаётся, а рекомендацию продолжает
  представлять уже существующая. Иначе два независимых документа, рекомендующих одно и то же,
  плодят текстуально неразличимые дубли, которые `medical.decline_planned_action` не может
  различить (см. ADR). Ключ без канонического имени (пустой `matchIndicatorName`/
  `matchProcedureName`) никогда не считается совпадением — иначе две разные, но одинаково
  "безымянные" рекомендации схлопнулись бы случайно. Дедуп всегда в разрезе `userId` — общая
  рекомендация двух разных членов семьи остаётся двумя отдельными записями, по одной на владельца.

---

# 6. Repository

`PlannedActionRepository`:

```text
Add(action PlannedAction) (PlannedAction, error)                         // self-reported: одна запись за вызов
ReplaceForSource(sourceType, sourceId string, actions []PlannedAction) error  // документы: удаляет только pending, дедуп по description внутри источника и по ключу между источниками, см. §5
RemoveBySource(sourceType, sourceId string) error                        // medical.delete_event
ListByUser(userId string) ([]PlannedAction, error)
ListPending(userId string) ([]PlannedAction, error)                      // кандидаты для planmatch/decline
ClearMatchesFromDocument(documentId string) error                        // сброс перед rematch, см. §5
MarkCompleted(id, matchedDocumentId, matchedEntityId string, at time.Time) error  // автозавершение, internal/planmatch
MarkDeclined(id, userId string) error                                     // medical.decline_planned_action
MarkCompletedManually(id, userId string, at time.Time) error              // medical.complete_planned_action — как MarkDeclined, но completed; matched* кроме matchedAt не трогает
```
