# birdman self-host — квикстарт

Postgres + birdman-master (REST-API и панель в одном образе, сборка из
исходников). Полная инструкция — `docs/self-host.md`; здесь только «поднять».

## Требования
- Docker + Docker Compose v2 (`docker compose version`).
- Клон репозитория (`git clone …` → `cd deploy`): образ собирается локально.

## Поднять
```bash
cp .env.example .env                                  # 1. задай POSTGRES_PASSWORD (не оставляй change-me)
umask 077 && openssl rand -base64 32 > secrets.key    # 2. ключ шифрования секретов at-rest
docker compose up -d --build                          # 3. собрать и запустить (postgres + master)
docker compose logs master | grep 'bootstrap admin'   # 4. admin-ключ (bmk_…) — показан ОДИН раз, сохрани
# открой в браузере: http://127.0.0.1:8100                             # 5. панель + REST (только localhost хоста)
```
`secrets.key` и admin-ключ — два секрета self-host; оба git-ignored. Потеря
`secrets.key` = секреты в БД не расшифровать → эскроу-копию в менеджер паролей.

Ноды, выпуск версий, reverse-proxy наружу, mTLS-энролл агентов — `docs/self-host.md`.

## Снос
```bash
docker compose down -v    # -v удаляет ВСЁ, включая том Postgres (базу) и secrets-том
```
