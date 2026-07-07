# birdman-master image for dev-compose. Build context = repo root
# (the master module needs ../proto for the replace directive):
#   docker build -f master/Dockerfile .
FROM golang:1.24 AS build
WORKDIR /src
COPY proto/ proto/
COPY master/ master/
WORKDIR /src/master
ENV CGO_ENABLED=0 GOFLAGS=-buildvcs=false
RUN go build -trimpath -ldflags '-s -w' -o /out/birdman-master ./cmd/birdman-master

FROM scratch
COPY --from=build /out/birdman-master /birdman-master
# /certs — volume for the self-signed dev TLS pair (auto-generated once).
ENTRYPOINT ["/birdman-master"]
CMD ["-config", ""]
