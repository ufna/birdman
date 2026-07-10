-- Секреты at-rest: registries.token и internal_ca.key_pem больше не plaintext.
-- Мастер шифрует их AEAD-конвертом (AES-256-GCM) перед записью и расшифровывает
-- после чтения (docs/superpowers/specs/2026-07-10-secrets-encryption-design.md
-- §3/§4); существующие строки шифрует одноразовый startup-проход. Схема НЕ
-- меняется: конверт birdman:v1:<key_id>:<base64(nonce||ct)> — валидный UTF-8 в
-- тех же text-колонках. Эта миграция правит ТОЛЬКО комментарии колонок —
-- прежние («plaintext», source-level в 000007/000008) после этого воркстрима
-- врут. Данных/типов/констрейнтов не трогает.
comment on column registries.token is
  'AEAD-конверт at-rest: birdman:v1:<key_id>:<base64(nonce||ct)>, AES-256-GCM, ключ /etc/birdman/secrets.key (secrets-encryption design). Расшифровывается мастером и раздаётся агентам по agentlink (SetRegistries) — транспорт загейчен mTLS/loopback; в GET/события/логи не отдаётся.';
comment on column internal_ca.key_pem is
  'AEAD-конверт at-rest: birdman:v1:<key_id>:<base64(nonce||ct)>, AES-256-GCM, ключ /etc/birdman/secrets.key (secrets-encryption design). Расшифровывается в память для подписи листов; в логи/%v и в plaintext-дамп не попадает.';
