# The community images disappeared from Docker Hub and Quay. Build the real
# S3 test fixture from a pinned upstream release instead of relying on mirrors.
FROM golang:1.25@sha256:699337d620559a59b4a2bb298ad59611e535d2ee755a34cf2d2a98f37578dc80 AS build
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOBIN=/out
# MinIO RELEASE.2025-10-15T17-29-55Z, the last community release.
RUN go install github.com/minio/minio@9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a \
    && mkdir /data

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/minio/minio" \
      org.opencontainers.image.revision="9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
COPY --from=build /out/minio /usr/local/bin/minio
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /go/pkg/mod/github.com/minio/minio@*/LICENSE /LICENSE
COPY --from=build --chown=65532:65532 /data /data
USER 65532:65532
ENV HOME=/data
EXPOSE 9000
ENTRYPOINT ["/usr/local/bin/minio"]
CMD ["server", "/data"]
