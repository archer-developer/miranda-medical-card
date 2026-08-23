## Проблема

`medical.profile` (`internal/mcpserver/profile.go`) отдаёт полный структурированный профиль (диагнозы,
медикаменты, аллергии, анализы, показатели, рекомендации по питанию) одним большим JSON прямо в теле ответа
инструмента — по дизайну, см. `docs/adr/002-structured-profile-response.md` (усечённая сводка вместо полного
JSON — уже задокументированная причина, по которой Миранда раньше строила неполные/неверно сгруппированные
PDF).

Обнаруженный на практике сценарий: пользователь просит Миранду собрать PDF из данных профиля.
`code-execution-sandbox` (соседний сервис, см. `../../miranda-code-execution-sandbox/CLAUDE.md`) уже умеет
рендерить такой PDF по фиксированному шаблону через baked-in скрипт (`medical_profile_pdf`), которому нужен
JSON-файл на диске внутри контейнера. Единственный способ доставить туда данные — модель должна **вручную
перепечатать** весь JSON, полученный от `medical.profile`, в аргумент `execute_in_session`/`upload_file`,
потому что `fileUri` (URI, а не сами байты) как формат ответа у `medical.profile` отсутствует — в отличие от
`medical.get_document`, где `fileUri` уже есть.

На реальном проде (`gemini-lite`, самый дешёвый провайдер) это воспроизводимо ломается: модель не копирует
JSON дословно, а **пересказывает его по памяти**, теряя большинство разделов (из 20 диагнозов/92 анализов
остаются единицы) и путая структуру между попытками. Это не проблема промпта/инструкций — большой JSON-блоб
физически относится к тому роду данных, которые языковая модель скорее суммаризирует, чем дословно
скопирует, вне зависимости от того, насколько явно об этом попросить.

## Идея

Дать `medical.profile` второй формат ответа — не сам JSON, а **короткоживущую ссылку** на него. Модель
получает маленькую строку (`fileUri`), которую можно передать `code-execution-sandbox`'s `upload_file`
как есть — тот сам делает `GET` за байтами сервер-сервер, и данные ни разу не проходят через
собственный контекст генерации модели, а значит не могут быть ни усечены, ни перепутаны.

Это тот же паттерн, что уже проверен в проде дважды — server-to-server передача файла по URI вместо байтов
в аргументе тула (`docs/mcp/02-files.md` §2, `medical.upload_document`) и random-ID+TTL как единственная
граница доступа (`miranda`'s `internal/attachments`, `miranda-code-execution-sandbox`'s
`internal/filestage.Stager`) — просто применённый к новому случаю: **эфемерные, производные данные**
(снэпшот профиля на момент запроса), а не постоянный документ пациента.

### Почему это НЕ то же самое, что "снять bearer-токен с `GET /files/{fileId}`"

Первая идея была проще — сделать существующий `/files/{fileId}` (`internal/mcpserver/files.go:52`)
безусловно публичным. Отклонена по двум причинам:

1. **Не тот security-профиль.** `/files/{fileId}` отдаёт постоянные, реальные документы пациента
   (результаты, выписки) бессрочно, пока жив `fileId` — сейчас это компенсируется bearer-токеном
   (`docs/adr/003-ownership-check-for-files-downloading.md` уже фиксирует известную дыру даже с токеном:
   `shared_with` не перепроверяется на каждый запрос). Убрать токен полностью означало бы сделать реальные
   медицинские документы доступными кому угодно, кто угадает/перехватит `fileId` — riskier, чем нужно для
   задачи «передать эфемерный снэпшот в песочницу за секунды».
2. **Не тот срок жизни.** `fileUri` от `medical.get_document` сегодня рассчитан жить неопределённо
   долго — Миranda's own `expose_files`-прокси (`../../miranda/internal/agent_loop/mcp_dispatch.go`,
   `detectRemoteFileLinks`) только ЗАПОМИНАЕТ этот URL в момент вызова тула, а реально фетчит его лениво,
   когда человек кликнет на чип скачивания в веб-UI — это может случиться через час после того, как чип
   появился. Пятиминутный TTL сломал бы этот рабочий сценарий. Новому случаю (модель немедленно, в рамках
   того же хода, передаёт ссылку в `upload_file`) пять минут — с большим запасом достаточно, а
   `medical.get_document`'s `fileUri` эту задачу не решает и трогать его не нужно.

Поэтому это отдельный, новый эндпоинт и отдельная таблица — не редизайн существующего `/files/{fileId}`.

## Что нужно сделать

1. **Новая таблица** `internal/storage/storage.go` (тот же inline-schema паттерн, что у `files`/
   `medical_documents`, строки 42-55):

   ```sql
   CREATE TABLE IF NOT EXISTS ephemeral_links (
       id           TEXT PRIMARY KEY,   -- случайный UUID (crypto/rand)
       file_id      TEXT,               -- NULL если content не NULL
       content      BLOB,               -- NULL если file_id не NULL
       content_type TEXT NOT NULL,
       filename     TEXT NOT NULL,
       created_at   INTEGER NOT NULL,
       expires_at   INTEGER NOT NULL
   );
   CREATE INDEX IF NOT EXISTS idx_ephemeral_links_expires_at ON ephemeral_links(expires_at);
   ```

   Ровно одно из `file_id`/`content` заполнено (инвариант на уровне приложения, не CHECK-констрейнта —
   упрощает миграции): `file_id` — ссылка на существующий `storage.File` (задел на будущее, если понадобится
   выдавать короткоживущие ссылки и на реальные документы — не требуется для этой задачи, но не стоит
   ничего, чтобы заложить сразу); `content` — сырые байты для производных данных, которых нет в постоянном
   хранилище (наш случай — сериализованный профиль).

2. **Новый пакет `internal/linkstore`** (по аналогии с `internal/filestore`):

   ```go
   func Mint(ctx context.Context, db *sql.DB, content []byte, contentType, filename string, ttl time.Duration) (linkID string, err error)
   func Resolve(ctx context.Context, db *sql.DB, linkID string) (content []byte, contentType, filename string, err error)
   ```

   `Resolve` проверяет `expires_at > now()`, иначе — та же ошибка, что "не найдено" (не различать
   «никогда не существовало» и «истекло» в ответе — не давать зацепку для тайминг-атак на TTL, тем более
   что различие не несёт полезной информации вызывающей стороне). Не реализован сейчас: разрешение через
   `file_id` (не нужно для профиля — оставить `TODO`/явную ошибку, чтобы не тащить недоделанный код).

3. **Периодическая зачистка** просроченных строк — тикер в `cmd/miranda-medical-card/main.go` рядом с
   остальной инициализацией (`DELETE FROM ephemeral_links WHERE expires_at < ?`), раз в несколько минут —
   TTL всего 5 минут, так что даже нечастая зачистка не даёт таблице расти заметно; хранить сами байты
   (`content BLOB`) в SQLite, а не на диске — при 5-минутном TTL и размере профиля (десятки КБ) не стоит
   заводить отдельный файловый стейджинг, как у `filestage.Stager` в песочнице (та живёт часами и хранит
   куда более крупные файлы).

4. **Новый HTTP-роут** `GET /links/{linkId}` — **без** `requireBearerToken` (`internal/httpserver/server.go`):

   ```go
   mux.Handle("GET /links/{linkId}", linkHandler) // не requireBearerToken — TTL+random UUID это и есть граница доступа
   ```

   Обработчик (новый `internal/mcpserver/links.go`, тем же паттерном, что `NewFileDownloadHandler` в
   `files.go:52-84`): читает `linkId`, `linkstore.Resolve`, стримит `content` с правильными
   `Content-Type`/`Content-Disposition`, 404 на отсутствующий/просроченный `id`.

   Отдельный путь, а не переиспользование `/files/{fileId}` — намеренно: у `code_exec_sandbox`'s
   `upload_file`/`../../miranda`'s `expose_files`-прокси нет причин путать «постоянный документ пациента»
   и «эфемерная производная выгрузка», и не совпадающий с `FilesEndpoint` конфига `medical_card` в
   `../../miranda/config/mcp.yaml` префикс URL — это заодно означает, что `detectRemoteFileLinks` не
   подхватит `/links/...` и не покажет пользователю бессмысленный чип «скачать сырой JSON профиля» — этот
   URI никогда не предназначен человеку, только для передачи между тулами в рамках одного хода.

5. **`medical.profile`** (`internal/mcpserver/profile.go`) — новый параметр формата:

   ```go
   type ProfileInput struct {
       UserID    string `json:"userId" jsonschema:"User identifier."`
       SubjectID string `json:"subjectId,omitempty" jsonschema:"..."`
       // Format selects the response shape: "json" (default) returns the
       // full profile inline, as today. "file_uri" instead stages the same
       // JSON behind a short-lived (5 min) link and returns just that URI —
       // use this when the caller's actual goal is to hand the data to
       // another tool (e.g. code-execution-sandbox's upload_file, for PDF
       // generation) rather than to read/quote values itself: retyping a
       // large profile by hand is unreliable (fields get dropped/summarized)
       // in a way passing a URI through cannot be.
       Format string `json:"format,omitempty" jsonschema:"Response shape: \"json\" (default, full profile inline) or \"file_uri\" (a short-lived link to the same JSON — use when handing this straight to another tool, e.g. PDF generation, instead of reading it yourself)."`
   }
   ```

   `registerProfileTool` (`server.go:52`) и `New` (`server.go:40`) должны прокинуть `publicBaseURL` и
   доступ к БД — тем же способом, каким `registerDocumentTools` уже получает `publicBaseURL`
   (`server.go:49`).

   `profileHandler`: при `Format == "file_uri"` — вместо обычного возврата сериализовать `ProfileOutput`
   в JSON, `linkstore.Mint(ctx, db, jsonBytes, "application/json", "profile.json", 5*time.Minute)`, вернуть
   маленькую структуру вместо полного профиля:

   ```go
   type ProfileFileOutput struct {
       FileURI   string `json:"fileUri"`
       ExpiresAt string `json:"expiresAt"` // RFC3339 — модель должна понимать срочность
   }
   ```

   (Технически это значит два разных выходных типа для одного тула в зависимости от параметра —
   `mcp.AddTool`'s generic `Output` не выражает такую вариативность напрямую; решить на этапе реализации,
   например через `any`/ручную сборку `CallToolResult` вместо типизированного `ToolHandlerFor`, как уже
   сделано в паре мест этого пакета для нестандартных ответов — не переусложнять специально под ADR.)

6. Обновить `docs/mcp/05-profile.md` — задокументировать `format`, форму `file_uri`-ответа, TTL, и то, что
   `/links/{linkId}` не требует токена (в отличие от `/files/{fileId}`).

7. Тесты: `internal/linkstore` — mint/resolve/expiry (сквозь реальные часы или инжектируемый clock, как уже
   принято в этом репо для TTL-логики — свериться с `internal/storage`'s существующими тестами на паттерн);
   HTTP-хендлер — 200 в пределах TTL, 404 после истечения и для неизвестного `id`; `profile_test.go` —
   `format=file_uri` отдаёт ссылку, по которой реально лежит тот же JSON, что `format=json` вернул бы
   инлайн (побайтовое сравнение, не выборочные поля — та же дисциплина, что уже требует
   `TestServer_Profile_ContentMirrorsStructuredContent` из `docs/adr/002`).

## Отношение к `docs/adr/003-ownership-check-for-files-downloading.md`

003 остаётся отдельной, всё ещё не реализованной проблемой — она про `/files/{fileId}` (постоянные
документы, ownership/`shared_with` может измениться после выдачи `fileUri`). Этот ADR её не решает и не
пытается: `/links/{linkId}` не про постоянные документы пациента и не про `shared_with` — минтится только
для собственного, только что запрошенного `medical.profile` вызывающего, и живёт 5 минут. Если позже
понадобится выдавать короткоживущие ссылки на реальные документы через `file_id`-путь `ephemeral_links`
(задел в п.1) — 003 всё равно останется актуальной для caller'ов `/files/{fileId}` напрямую.

## Безопасность — почему TTL+random UUID достаточно здесь

- То же самое устройство, что уже проверено в проде: `crypto/rand`-случайный ID (128 бит) + TTL как единая
  граница доступа — `miranda`'s `internal/attachments` (для скачиваний, инициированных самой Мирандой) и
  `miranda-code-execution-sandbox`'s `internal/filestage.Stager` (для `download_file`). Не новый,
  непроверенный паттерн.
- 5 минут — на пять порядков меньше, чем практическое окно подбора 128-битного ID перебором; сама задача
  (передать ссылку из одного tool-call в другой в рамках одного хода агентного цикла — обычно секунды)
  этого с большим запасом достаточно.
- Что именно минтится — решает только сам `medical_card` (никогда не по запросу извне, id не выбирается
  вызывающим) — та же модель доверия, что и у выдачи bearer-токена сегодня, только с истечением вместо
  бессрочности.
- `/links/{linkId}` наследует ту же сетевую границу, что и `/mcp`/`/files/{fileId}` — сервис слушает только
  `127.0.0.1` (см. `../../miranda/config/mcp.yaml`'s `url: https://127.0.0.1:8791/mcp`), так что
  "без токена" не значит "доступно откуда угодно" — доступно только тому, что уже может достучаться до
  localhost этого хоста (в проде — только `code-execution-sandbox`, который по этой же причине физически
  сидит на той же машине).

Статус: не реализовано, только план.
