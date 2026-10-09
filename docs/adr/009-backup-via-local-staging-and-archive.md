# Бэкап базы через локальный временный каталог и архив

> **Статус**: реализовано — см. `internal/backup`, `config.BackupConfig`'s `temp_dir`/`include_files`,
> `config/config.yaml.dist`. Идентичный механизм реализован в `miranda`'s `internal/backup` — оба
> проекта чинились одновременно, так как причина сбоя и решение полностью совпадают.

## Контекст

Встроенный бэкап (`internal/backup.Run` → `snapshot`) делал снимок SQLite-базы командой
`VACUUM INTO '<backup.dir>/<YYYYMMDD-HHMMSS>/medical-card.db'`, затем `copyDir` копировал рядом
`data/files`, а `prune` удалял старые снимки по `retention_count`.

`backup.dir` указывает на сетевой каталог: Synology NAS `//192.168.1.64/miranda`, примонтированный
по CIFS/SMB3 в `~/.backups` (`/home/archer/.backups/miranda-medical-card/backups`).

С 2026-09-23 каждый запуск бэкапа падал:

```
backup failed: snapshot database: vacuum data/medical-card.db into
/home/archer/.backups/miranda-medical-card/<ts>/medical-card.db:
database is locked (5) (SQLITE_BUSY)
```

Проверено вручную: `VACUUM INTO` в локальный каталог проходит, а в каталог на CIFS — `database is
locked`. SQLite не может взять блокировку на файле назначения на сетевой ФС — ограничение SQLite и
SMB, не ошибка в коде. За две недели не было создано ни одного бэкапа.

То же самое оказалось верно и для `miranda`: её `backup.dir` смонтирован на тот же NAS
(`/home/archer/.backups/miranda`), и последний успешный бэкап там датирован 28 августа — после
этого каждый запуск молча падал по той же причине.

Дополнительно `data/files` в medical-card сам примонтирован с NAS
(`//192.168.1.64/miranda/miranda-medical-card/files`). Копирование `files` в каждый снимок — это
копирование NAS → NAS по сети: файлы и так лежат на NAS, а снимок только удваивает объём передачи.

## Решение

Бэкап никогда не пишет SQLite-файлы напрямую в `backup.dir`. Порядок (`internal/backup.Run`):

1. Создать временный каталог на локальном диске (`os.MkdirTemp(backupCfg.TempDir, ...)`,
   `backup.temp_dir` пусто → `os.TempDir()`). Удаляется через `defer os.RemoveAll`.
2. `VACUUM INTO` каждую существующую SQLite-базу в этот локальный каталог.
3. `PRAGMA integrity_check` на каждом снимке — результат должен быть `ok`, иначе весь запуск
   считается неудачным (`step=verify`).
4. Скопировать `config/*.yaml` и `.env` туда же (и `files.dir`, только если включён
   `backup.include_files` — см. ниже).
5. Записать `manifest.json` (время, версия приложения из `vcs.revision`, размер и SHA-256 каждой
   базы) и собрать всё в `<name>-<YYYYMMDD-HHMMSS>.tar.gz`.
6. Скопировать архив в `backup.dir` атомарно: записать `<name>.tar.gz.part`, `fsync`, пересчитать
   размер и SHA-256 уже записанного `.part`-файла и сравнить с локальным архивом, затем
   `os.Rename` в `<name>.tar.gz`. Rename внутри одной SMB-папки работает; сама запись — обычные
   `os.Create`/`io.Copy`, без блокировок SQLite, поэтому CIFS не мешает.
7. `prune` работает по файлам `<name>-*.tar.gz` (а не по подкаталогам), оставляя
   `retention_count` самых новых. `*.tar.gz.part`-файлы старше 24 часов удаляются как
   осиротевшие после упавшего запуска.

### Каталог files (только medical-card)

По умолчанию `data/files` в архив **не** включается (`backup.include_files: false`) — он уже живёт
на NAS. Если включить, файлы добавляются в тот же архив прямо из `files.dir`, с пропуском
`*.tmp`-файлов недозаписанных загрузок (как и раньше).

### Наблюдаемость

- Любой шаг с ошибкой → `level=ERROR msg="backup failed" step=<stage|snapshot|verify|archive|upload|prune>`,
  временные файлы удаляются, уже существующие в `backup.dir` архивы не трогаются.
- Успешный запуск → `level=INFO msg="backup ok" path=... bytes=... sha256=...`.

## Последствия

Плюсы:
- Бэкап работает на любом назначении: CIFS, NFS, локальный диск — ровно то, что было сломано.
- Один файл на снимок с проверкой целостности, атомарное появление в назначении — в `backup.dir`
  не бывает «полубэкапа».
- Нет бессмысленного копирования `files` NAS → NAS по умолчанию.
- Базы небольшие, в gzip ещё меньше — хранить много архивов дёшево.

Минусы:
- Нужен свободный локальный диск размером с базу(ами) на время каждого запуска.
- Старый формат (каталог `<ts>/` с файлами внутри) несовместим с новым `prune`, который ищет
  `*.tar.gz`. Успешных бэкапов за последние недели не было ни у одного проекта — миграция данных
  не нужна, старые пустые/сломанные каталоги можно удалить руками с NAS при следующей уборке.
- Восстановление — `tar xzf` + остановка сервиса + подмена файла базы (см. ниже).

## Альтернативы

Те же, что были рассмотрены и отклонены в первоначальном черновике:

- **Внешний скрипт + systemd-таймер** — отклонено, логика бэкапа размазывается между приложением
  и хостом.
- **`sqlite3_backup_*` напрямую в CIFS** — та же блокировка на файле назначения, не решает проблему.
- **Держать базу на NAS** — SQLite официально не поддерживает WAL/блокировки на сетевых ФС.

## Восстановление

```bash
tar xzf medical-card-20261009-120000.tar.gz -C /tmp/restore
cat /tmp/restore/manifest.json                              # что внутри
sqlite3 /tmp/restore/medical-card.db 'PRAGMA integrity_check'  # должно быть "ok"

systemctl --user stop miranda-medical-card    # на сервере — сначала остановить процесс
cp /tmp/restore/medical-card.db ~/miranda-medical-card/data/medical-card.db
cp /tmp/restore/.env ~/miranda-medical-card/.env
cp -r /tmp/restore/config/. ~/miranda-medical-card/config/
# если архив собран с include_files: true —
# cp -r /tmp/restore/files/. ~/miranda-medical-card/data/files/
systemctl --user start miranda-medical-card
```

## Тесты

`internal/backup/backup_test.go` (оба проекта): архив распаковывается и база проходит
`integrity_check` и совпадает с исходником по данным; `files.dir` не попадает в архив, когда
`include_files` выключен (по умолчанию), и попадает (без `*.tmp`) когда включён; `prune` оставляет
N архивов и удаляет старые `.part`; повреждённый источник (не читается как SQLite) останавливает
весь запуск и не создаёт файл в `backup.dir`; `uploadAtomic` откатывает `.part`-файл при
расхождении размера/SHA-256 после копирования.

Ручная проверка на сервере — см. "Восстановление" выше плюс
`ssh archer@miranda '~/miranda-medical-card/miranda-medical-card backup'` (или `~/miranda/miranda
backup`) и `ls ~/.backups/.../backups`.
