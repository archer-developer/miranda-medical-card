## Проблема

`GET /files/{fileId}` (`internal/mcpserver/files.go`, `NewFileDownloadHandler`) авторизует запрос только общим bearer
token'ом и не перепроверяет ownership/`shared_with` на каждый запрос — в отличие от `medical.download_file`
(`downloadFileHandler`), который на каждый вызов заново проходит `gate.resolveOwnersToTry` и берёт файл через
owner-scoped `pl.DownloadFile(ctx, ownerID, fileID)`.

Это осознанный trade-off, задокументированный прямо в `files.go` (строки 26-40): `shared_with` проверяется один раз, в
момент, когда `medical.get_document` формирует `fileUri` (`gate.resolveOwnersToTry(in.UserID)` в `documents.go:348`) —
дальше сам URI никаких прав не несёт и не истекает.

Последствие: если владелец делится документом (добавляет пользователя в `shared_with`), тот вызывает
`medical.get_document` и получает `fileUri`, а затем владелец отзывает доступ (убирает из `shared_with`) —
ранее выданный `fileUri` продолжает работать бессрочно, потому что `DownloadFileByID`/`GetByID`
(`internal/storage/file.go:104`) не делает per-request проверку владения. Отзыв `shared_with` для уже выданных
файлов не работает, хотя `medical.download_file` для тех же файлов такой отзыв уважает.

## Идея

Привести `GET /files/{fileId}` к той же гарантии, что уже есть у `medical.download_file`: перепроверять
ownership/`shared_with` на каждый HTTP-запрос, а не только в момент выдачи `fileUri`.

Для этого HTTP-путь должен знать, от чьего имени сделан запрос — сейчас `fileId` в URL никак не привязан к
запрашивающему пользователю, это и есть корень проблемы.

## Что нужно сделать

1. **`fileURI`** (`internal/mcpserver/files.go:122`) — добавить параметр `userID`, зашить его в URL как query-параметр:
   `fileURI(publicBaseURL, fileID, userID)` → `.../files/{fileId}?userId=...`. Путь роута (`GET /files/{fileId}`)
   менять не нужно.
2. **`documents.go:363`** (`getDocumentHandler`) — прокинуть `in.UserID` в `fileURI`.
3. **`NewFileDownloadHandler`** (`files.go:52`) — принять `*userGate` дополнительным параметром; читать `userId` из
   query-строки, вызывать `gate.requireUser(userId)`, затем вместо безусловного `pl.DownloadFileByID(ctx, fileID)`
   пройти `gate.resolveOwnersToTry(userId)` и брать файл через owner-scoped `pl.DownloadFile(ctx, ownerID, fileID)` —
   тот же паттерн, что уже есть в `downloadFileHandler` (`files.go:165-175`).
4. **`cmd/miranda-medical-card/main.go:234`** — передать `gate` в `NewFileDownloadHandler`.
5. Обновить doc-комментарий `files.go:26-40` — обоснование «проверяется один раз, в момент выдачи URI» больше не
   актуально, оба пути теперь перепроверяют доступ на каждый запрос.
6. Обновить `internal/mcpserver/server_test.go:237` (сигнатура `NewFileDownloadHandler` меняется) и добавить тест на
   отзыв доступа: получить `fileUri` при активном `shared_with`, убрать пользователя из `shared_with`, убедиться, что
   повторный `GET` по тому же `fileUri` возвращает 404/403.

Стоимость: один дополнительный owner-scoped lookup в БД на скачивание файла — дёшево при размере домохозяйства
(см. обоснование `resolveOwnersToTry` в `internal/mcpserver/users.go:72-80`).

Статус: не реализовано, только план.
