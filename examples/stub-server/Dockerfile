FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /stub-server .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /stub-server /stub-server
ENTRYPOINT ["/stub-server"]
