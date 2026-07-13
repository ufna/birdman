# birdman-master image: панель (vite) вкомпилена через go:embed — self-host
# получает продукт целиком, не placeholder. Контекст сборки = корень репо
# (master module тянет ../proto через replace):
#   docker build -f master/Dockerfile .
# Потребители: master/dev-compose.yml (дев-стенд) и deploy/compose.yml
# (self-host). Runtime — alpine (не scratch): healthcheck wget'ом и отладка
# стоят +8MB (спека selfhost-v1 §1).

FROM node:22 AS panel
WORKDIR /src/panel
COPY panel/package.json panel/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY panel/ ./
RUN npm run build

FROM golang:1.24 AS build
WORKDIR /src
COPY proto/ proto/
COPY master/ master/
COPY --from=panel /src/panel/dist/ master/internal/panelui/static/
WORKDIR /src/master
ENV CGO_ENABLED=0 GOFLAGS=-buildvcs=false
RUN go build -trimpath -ldflags '-s -w' -o /out/birdman-master ./cmd/birdman-master

FROM alpine:3.22
# pg_dump для Backups v1: master сам дампит БД (spec 2026-07-13). Версия
# клиента = мажору postgres:16 из deploy/compose.yml.
RUN apk add --no-cache postgresql16-client
RUN adduser -D -H -u 10001 birdman
COPY --from=build /out/birdman-master /birdman-master
ENTRYPOINT ["/birdman-master"]
CMD ["-config", ""]
