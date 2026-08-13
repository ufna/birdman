# proto — контракты birdman

Go-модуль `github.com/ufna/birdman/proto`. Контракт master↔agent: `agentlink/v1/agentlink.proto`
(спека: `docs/specs/protocol.md` §1). Сгенерированный Go-код (`*.pb.go`) **закоммичен** —
потребителям модуля генератор не нужен.

## Семантика

- Агент дозванивается до master сам (outbound), первый `AgentMsg` в стриме — `Hello{node_token}`.
- Каждая команда master несёт `cmd_id`; агент подтверждает `Ack{cmd_id}`.
  Master ре-шлёт неподтверждённые команды при реконнекте → обработка команд на агенте
  должна быть идемпотентна по `cmd_id` (at-least-once).
- Поля только добавляем, номера не переиспользуем (`reserved` при удалении).
  Пакет `agentlink.v1` — мажор контракта; breaking change = новый `v2` рядом.

## Генерация (воспроизводимо, Go на хосте не нужен)

Из каталога `proto/`:

```sh
# 1. одноразово: образ с buf + пинованными protoc-плагинами (см. protogen.Dockerfile)
docker build -t birdman-protogen -f protogen.Dockerfile .

# 2. генерация (перезаписывает agentlink/v1/*.pb.go)
docker run --rm -v "$PWD":/workspace birdman-protogen generate

# 3. линт контракта (правила и список исключений — buf.yaml)
docker run --rm -v "$PWD":/workspace birdman-protogen lint
```

Шаги 2 и 3 — не только для человека: их гоняет CI (`.github/workflows/proto.yml`,
джоб `contract-gates`) тем же образом и теми же командами, так что расхождение
`.proto` ↔ `.pb.go` и нарушение правил `buf.yaml` роняют сборку, а не ждут, пока
кто-нибудь запустит линт руками. Секция `breaking` из `buf.yaml` пока НЕ гоняется
нигде — ей нужен baseline-ref, это отдельный разговор.

Версии генераторов пинованы: `protoc-gen-go v1.36.6`, `protoc-gen-go-grpc v1.5.1`,
`buf 1.55.1` (см. `protogen.Dockerfile`, `buf.gen.yaml`). После генерации:

```sh
docker run --rm -v "$PWD":/src -w /src/proto golang:1.24 go build ./...
```
