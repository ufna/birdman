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

# 4. совместимость с точкой заморозки v1 (секция breaking в buf.yaml).
#    База — коммит рождения мажора, а не «предыдущий»: внутри v1
#    несовместимости не бывает НИКОГДА, см. ниже.
#    Pathspec `:/proto` — от корня репозитория, а не от текущего каталога:
#    команда запускается ИЗ proto/, и голое `proto` тут не нашлось бы.
base="$(mktemp -d)"
git archive 2d4ec93f68e4cd30b5bfe09c5f0f2adb93f5bded :/proto -o "$base/freeze.tar"
tar -x -C "$base" -f "$base/freeze.tar"
docker run --rm -v "$PWD":/workspace -v "$base":/baseline:ro \
  birdman-protogen breaking . --against /baseline
```

Шаги 2–4 — не только для человека: их гоняет CI (`.github/workflows/proto.yml`,
джоб `contract-gates`) тем же образом и теми же командами, так что расхождение
`.proto` ↔ `.pb.go`, нарушение правил `buf.yaml` и несовместимая правка контракта
роняют сборку, а не ждут, пока кто-нибудь запустит проверку руками.

**База для `breaking` неподвижна и это осознанно** (tracker #1116). `agentlink.v1`
объявлен мажором контракта: несовместимости внутри него не бывает никогда, а
ломать — значит заводить `v2` рядом. Скользящая база (`--against` на `main`)
отвечала бы на другой вопрос — «не сломали ли с прошлого раза» — и на `push: main`
сравнивала бы дерево с самим собой, то есть молчала бы ровно на том пути, которым
сюда чаще всего и приезжают правки. Точка заморозки — коммит
`2d4ec93f68e4cd30b5bfe09c5f0f2adb93f5bded` («proto: контракт agentlink v1»);
на сегодняшнем дереве проверка зелёная — весь рост контракта с тех пор строго
аддитивен. Сдвинуть базу можно только правкой этой строки и шага в workflow.

Версии генераторов пинованы: `protoc-gen-go v1.36.6`, `protoc-gen-go-grpc v1.5.1`,
`buf 1.55.1` (см. `protogen.Dockerfile`, `buf.gen.yaml`). После генерации:

```sh
docker run --rm -v "$PWD":/src -w /src/proto golang:1.24 go build ./...
```
