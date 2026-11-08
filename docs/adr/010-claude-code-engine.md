# Claude Code как движок `medical.ask` и как LLM-провайдер конвейера

> **Статус**: реализовано (2026-11-08). Переключается в конфиге, по умолчанию выключено
> (`agent.engine: miranda`, провайдеры `gemini`/`anthropic`). Проверено против реального
> Claude Code CLI 2.1.286 под подпиской (pro) на локальной копии базы — см. §6.
>
> Код: `miranda-llm/claudecode` (процесс, протокол, one-shot провайдер, трассировка),
> `internal/ask/engine.go` + `internal/ask/claude_code_engine.go` (движок), `internal/config`
> (`agent:`, `type: claude_code`), `cmd/miranda-medical-card/main.go`, `cmd/medical-dev`.

---

## 1. Контекст

Miranda уже умеет отдавать ход headless-процессу Claude Code
(`miranda/docs/adr/claude-code-orchestrator.md`): рассуждение и выбор тулов делает
`claude -p --input-format stream-json --output-format stream-json`, залогиненный через
claude.ai, а тулы остаются Miranda'иными и доступны процессу через loopback MCP. Главный выигрыш —
**подписка вместо API-токенов**.

В Medical Service LLM вызывается в двух независимых подсистемах (см. `CLAUDE.md`,
`05-llm.md`):

- **Document Pipeline** — однократные вызовы `Chat` (OCR, Stage 1) и `Structured`
  (Structured Extraction, инструментальные находки, self-reported events, decline matching,
  nutrition, сверка диагнозов) через `llm.ocr_provider`/`llm.extraction_provider`;
- **Agent Loop** (`medical.ask`) — открытый многоходовый `Chat` с Knowledge Providers как
  тулами, через `router.Router` над `llm.agent_provider`.

Нужно перевести обе подсистемы на Claude Code, сохранив возможность вернуться на прежние
провайдеры одной строкой конфига.

## 2. Решение в двух частях

Как и в Miranda, шов проходит в двух разных местах — потому что у подсистем разная природа:

| | Document Pipeline | `medical.ask` |
|---|---|---|
| Форма вызова | один запрос → один ответ, без тулов | модель сама крутит цикл «модель → тул → модель» |
| Шов | **LLM-провайдер** (`llm.Provider` + `llm.StructuredProvider`) | **движок** (`internal/ask.engine`) — заменяется весь цикл, а не один `Chat()` |
| Конфиг | `llm.providers[].type: claude_code` | `agent.engine: claude_code` |
| Процесс | свежий на каждый вызов, умирает после ответа | свежий на каждый вопрос, умирает после ответа |

## 3. Пакет `miranda-llm/claudecode`

Общий код живёт в `miranda-llm`, а не в медкарте: это та же «LLM-плумбинг», что и
`gemini`/`anthropic`, и он нужен как минимум двум сервисам.

- **`Process`** (`process.go`) — перенесён из `miranda/internal/claudecode` почти без изменений
  (там флаги и протокол уже проверены на реальном CLI): `--no-session-persistence`,
  `--strict-mcp-config`, `--setting-sources ""`, `--disable-slash-commands`,
  `--system-prompt-file`, очистка `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`/`CLAUDECODE` из
  окружения, приватный 0700 рабочий каталог, удаляемый при `Close`, kill по `ctx`. Отличия:
  - **вложения**: `Attachment` вместо `Image` — `image/*` уходит `image`-блоком,
    `application/pdf` — `document`-блоком Messages API. Проверено на реальном CLI: многостраничный
    скан PDF распознаётся;
  - **`JSONSchema`** → `--json-schema`: CLI заставляет модель ответить валидированным вызовом
    тула `StructuredOutput` и кладёт значение в `structured_output` result-события
    (`Result.StructuredOutput`). Это отдельный ход модели, поэтому `max_turns` ≥ 2;
  - **`ToolSearch` не добавляется неявно** — `BuiltinTools` передаётся как есть (пусто = ни
    одного встроенного тула). В Miranda он всегда включён из-за 120 MCP-тулов; у медкарты их ~11,
    и откладывать схемы ради экономии нет смысла;
  - `Effort` → `--effort`.
- **`Provider`** (`provider.go`) — `llm.Provider` + `llm.StructuredProvider` для однократных
  вызовов. Каждый `Chat`/`Structured` — `Start` → один `Turn` → `Close`: **процесс живёт ровно
  один вызов** и умирает, как только ответил. Ничего не переживает вызов, поэтому параллельные
  вызовы полностью независимы. Ограничения, вытекающие из формы one-shot CLI:
  - тулов нет — `Chat` с `Tools` возвращает `ErrToolsUnsupported` (вызвать тул и вернуть
    управление вызывающему посреди потока, как требует контракт `llm.Provider`, CLI не может);
  - `Temperature` игнорируется — у CLI нет параметров сэмплинга;
  - `Chat` не стримит — ответ приходит одним чанком по завершении (OCR всё равно читает текст
    целиком);
  - системные сообщения склеиваются в `--system-prompt-file`; если не-системных сообщений
    больше одного, предыдущие рендерятся транскриптом перед последним.
  Таймаут вызова — `claude_code.timeout_seconds` (по умолчанию 5 минут, процесс убивается).
- **`TraceBuilder`** (`trace.go`) — рендер многошагового `Turn` в блоки `llm.log` в форме
  Anthropic Messages, один блок на вызов модели внутри CLI. Граница вызова — смена `message.id`
  ассистента, а не приход tool results: CLI начинает исполнять tool call сразу, как пришёл его
  блок, поэтому при параллельных вызовах результаты первых тулов перемежаются с последующими
  `tool_use` того же ответа (поймано на реальном CLI — первая версия резала один ход на
  несколько блоков). Follow-up запрос рендерится как `[assistant: прошлый ответ, user:
  результаты]` — иначе `analyze.IsFirstTurn` принимает его за новый вопрос, и
  `llm-trace --untagged` разбивает один вопрос на несколько. Префикс `mcp__<server>__` у имён
  тулов снимается, чтобы детектор аномалий не считал каждый вызов неизвестным тулом.
  Base64 вложений в трассе заменяется размером.

Попутно `llmtrace/analyze` научился читать `tool_result.content` в виде массива блоков
(`[{"type":"text","text":...}]`) — в такой форме CLI отдаёт результаты MCP-тулов. Без этого
`llm-trace` показывал пустые результаты, а детектор `tool_error` не видел `error: ...` — это
касалось и трасс Miranda'иного оркестратора.

`miranda/internal/claudecode` пока остаётся собственной копией Miranda: перевод Miranda на
`miranda-llm/claudecode` — отдельная задача (см. §8).

## 4. Провайдер для Document Pipeline

```yaml
llm:
  providers:
    - name: claude-code
      type: claude_code
      model: sonnet                 # --model: sonnet | opus | haiku | полный id
      claude_code:
        binary: /home/archer/.local/bin/claude   # PATH systemd --user юнита не содержит ~/.local/bin
        timeout_seconds: 600
        # effort: high
        # allow_api_key_env: false
        # extra_args: []
  ocr_provider: claude-code
  extraction_provider: claude-code
```

`api_key_envs` для этого типа не нужен (и не проверяется) — CLI использует свой логин.
`escalation.target_provider` работает как у любого другого провайдера: OCR при жёсткой ошибке и
Structured Extraction при подозрительно пустом результате один раз повторяются против цели — так
что упёршийся в лимит подписки вызов (`is_error` result, например «usage limit reached») можно
подстраховать Gemini.

Обратное — tool-based эскалация (`escalation.tool_name`) *в* `claude_code`-провайдер —
`config.validate` отклоняет: она передаёт открытый диалог с тулами внутри `router.Router`, а
`claude_code` тулы не умеет и в роутер не входит. Контентная эскалация (без `tool_name`) в
`claude_code` допустима.

`claude_code`-провайдер **не может быть `agent_provider`** — `config.validate` это отклоняет
(даже при `agent.engine: claude_code`, чтобы переключение движка обратно никогда не поднимало
сломанный цикл), а `buildAskRouter` исключает такие провайдеры из fallback-цепочки агента; их
трассировщик ставится напрямую (`setTracer`).

## 5. Движок `medical.ask`

### 5.1 Шов

`internal/ask.engine` (`engine.go`) — аналог Miranda'иного `turnEngine`. `Asker.Ask` остаётся
общим для обоих движков: per-session lock, anomaly `Recorder`, загрузка истории сессии, запись
вопроса и финального ответа, системный промпт, набор тулов, сбор `sources` из показанных модели
чанков. Движок только рассуждает и вызывает тулы — через общий `Asker.callTool`
(`executeToolCall` + запись результата в сессию + накопление чанков). Поэтому переключение
`agent.engine` меняет только то, *как* ищется ответ, а не что сохраняется и возвращается.

- `mirandaEngine` — прежний цикл над `router.Router` без изменений поведения.
- `claudeCodeEngine` — новый; включается `Asker.EnableClaudeCode` из `main.go`
  (`agent.engine: claude_code`). Роутер строится в обоих режимах.

### 5.2 Процесс на вопрос, а не на пользователя

В отличие от Miranda (долгоживущий процесс на пользователя, реюз между ходами, idle-reaping,
правила перезапуска), здесь **процесс живёт ровно один вызов `medical.ask`** и закрывается
сразу после ответа:

- `medical.ask` вызывается редко, и вопросы независимы — выигрыш тёплого контекста мал;
- `sessionId` необязателен, а источник правды об истории — `SessionStore` в любом случае: свежий
  процесс получает предыдущие ходы сессии транскриптом (`<previous_conversation>`, результаты
  тулов обрезаны до 1000 символов);
- процесс, который никогда не переживает вызов, не требует reaping'а, ключей сессий и правил
  «когда перезапускать», и не может протащить состояние одного вопроса (другого субъекта!) в
  другой.

Цена — холодный старт на каждый вопрос (≈3–5 с поверх ответа).

### 5.3 Medical MCP

Loopback HTTP-сервер (`127.0.0.1:<random>`, go-sdk streamable handler), поднимается один раз
в `EnableClaudeCode`. На каждый вызов — свой `*mcp.Server` со своим HTTP-хэндлером и свой
случайный 256-битный bearer-токен; токен однозначно определяет вызов, а с ним — `subjectID`,
поэтому процесс не может прочитать данные другого субъекта. Без токена или с чужим — 401. Когда
вызов заканчивается, токен снимается, а сервер и хэндлер (вместе с MCP-сессией SDK) просто
отбрасываются.

Тулы — ровно `toolDefsForRegistry(registry)` (те же схемы, что у `mirandaEngine`); процесс видит
их как `mcp__medical__<name>`, о чём системному промпту дописывается одна строка. **Встроенных
тулов нет вообще** (ни веба, ни файлов): ответ о медицинской истории должен опираться только на
записи пользователя.

`UserID` в `KnowledgeRequest` по-прежнему ставится из MCP-аутентифицированного `subjectID`
(`executeToolCall`), никогда из аргументов модели. Вызовы тулов внутри одного вопроса
сериализуются (`toolMu`): CLI может звать MCP параллельно, а история сессии должна сохранять
порядок «вызов → результат». id вызова — `_meta.claudecode/toolUseId` от CLI, так что история и
`llm.log` ссылаются на одно и то же.

### 5.4 История, трассировка, аномалии

- **История** пишется как у `mirandaEngine`: assistant-сообщение с `tool_calls` → `tool`-результат
  → финальный ответ. Разговор можно продолжить, переключив движок обратно. Текст, который модель
  пишет *между* вызовами, не сохраняется (он идёт по stdout, вызов — по MCP).
- **`llm.log`** — `TraceBuilder`, `provider=claude_code`; трейсер тот же `llmtrace.ContextTracer`,
  что у роутера, поэтому блоки доходят и до per-turn anomaly `Recorder`. `medical-dev llm-trace`
  работает без изменений.
- **`Outcome`**: `IterationCount = num_turns` (при таймауте/падении, когда result-события нет, —
  число вызовов модели, увиденных `TraceBuilder.Calls`), `MaxIterations =
  agent.claude_code.max_turns`; `error_max_turns` → `HitIterationCap`; таймаут → `TimedOut`.
  Отказ тула после конца вызова начинается с `error:`, как ошибки `executeToolCall`, чтобы его
  видел детектор `tool_error`.

### 5.5 Конфигурация

```yaml
agent:
  engine: claude_code      # miranda (по умолчанию) | claude_code
  claude_code:
    binary: /home/archer/.local/bin/claude
    model: sonnet          # по умолчанию
    max_turns: 16          # по умолчанию, как askMaxToolIterations
    timeout_seconds: 300   # по умолчанию
    # effort, allow_api_key_env, extra_args — как у провайдера
```

Неизвестное значение `engine` — ошибка старта, а не тихий откат на `miranda`. При старте
сервис один раз (в фоне, не задерживая старт HTTP-сервера) проверяет `claude auth status` для
каждого используемого бинаря (`logClaudeCodeAuth`) и пишет WARN, если CLI не залогинен.
Маппинг конфига в `claudecode.Config`/`ask.ClaudeCodeConfig` — один на оба бинаря
(`config.ProviderConfig.ClaudeCodeConfig`, `ask.ClaudeCodeConfigFrom`), чтобы `medical-dev` не
мог разойтись с сервисом.

## 6. Проверка

- `go test ./claudecode/` (miranda-llm) — протокол, жизненный цикл и провайдер на фейковом CLI
  (self-exec тестового бинаря): процесс умирает после `Close`, kill по таймауту, stderr при
  падении, удаление рабочего каталога, флаги изоляции, очистка env, `--json-schema` →
  `structured_output`, отказ при тулах, ошибки CLI (`is_error`, `error_max_turns`, падение),
  транскрипт, трасса без base64, группировка параллельных вызовов по `message.id`.
- `go test ./internal/ask/ -run ClaudeCode` — сквозной путь на фейковом CLI, который реально ходит
  в Medical MCP по HTTP: вызов тула под нужным субъектом, `sources`, процесс мёртв после `Ask`,
  история с id от CLI, транскрипт для второго вопроса сессии, `error_max_turns` → аномалия
  `iteration_cap`, 401 на чужой токен, формат `llm.log`.
- Реальный CLI (sonnet, подписка pro), локальная копия базы, `medical-dev`:
  - `pipeline --provider claude-code <doc>` для двух PDF-сканов (УЗИ мягких тканей, консультация):
    OCR, Structured Extraction, инструментальные находки, nutrition — все через Claude Code,
    статус `READY`. OCR оказался полнее прежнего: в УЗИ появились заключение, рекомендация
    («Консультация травматолога-ортопеда» → новый PlannedAction) и врач, которых сохранённый
    текст не содержал. Вызовы — 2–20 с, весь документ — 30–50 с (включая эмбеддинги Gemini).
  - `ask --engine claude_code "…холестерин… что назначил врач…"` — 3 хода модели, параллельные
    вызовы `lab_results`/`procedures`/`medications`, затем `documents`/`timeline`, корректный
    ответ с источниками за ~10 с.

## 7. Компромиссы и открытые вопросы

- **Латентность.** Холодный старт процесса ≈3–5 с на каждый вызов LLM в конвейере и на каждый
  вопрос. Для конвейера (синхронный `upload_document`, десяток вызовов) это заметно, но не
  критично; для `medical.ask` — секунды поверх ответа.
- **Лимиты подписки** общие с Miranda'иным оркестратором и интерактивной работой владельца в
  Claude Code. Обработка пачки документов может съесть заметную долю 5-часового окна.
  Подстраховка — `escalation.target_provider` на Gemini для этапов конвейера.
- **Условия подписки.** Технически работает; допустимость обслуживания семьи через личную
  подписку — вопрос условий Anthropic, не кода (как и в Miranda).
- **Температура** не управляется — прежний аргумент про низкую температуру OCR/Structured
  (`ocrTemperature`, `defaultStructuredTemperature`) к этому провайдеру неприменим.
- **`agent_provider` остаётся обязательным** и при `agent.engine: claude_code` — роутер
  строится всегда; это сохраняет мгновенный откат и не плодит условных путей в `validate`.
- **Изображения** (`image/*`) в конвейере на реальном CLI не проверялись — только PDF; формат
  блока тот же, что Miranda использует для изображений.
- Продакшен: на сервере нужен установленный и залогиненный `claude` под пользователем
  systemd-юнита (уже есть — им пользуется Miranda), путь задаётся явно.

## 8. Дальше

- Перевести Miranda с `miranda/internal/claudecode` на `miranda-llm/claudecode` (изменения
  обратно совместимы, кроме явного `ToolSearch` в `BuiltinTools` и нового
  `NewTraceBuilder(tracer, …)`/`AddResponse(ctx, id, b)`).
- Если холодный старт `medical.ask` станет проблемой — долгоживущий процесс на `sessionId` по
  образцу Miranda.
