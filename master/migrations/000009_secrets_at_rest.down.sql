-- Возврат к прежнему состоянию комментариев: у token/key_pem не было настоящего
-- DB-COMMENT (лишь source-level «plaintext» в 000007/000008), поэтому down
-- просто снимает комментарии, добавленные up. Данные остаются зашифрованными —
-- миграция их не касается (откат шифрования — это откат бинаря + restore
-- пре-апгрейдного дампа, design §Принятые ограничения п.3, не задача down).
comment on column registries.token is null;
comment on column internal_ca.key_pem is null;
