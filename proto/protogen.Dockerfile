# Reproducible protoc/buf toolchain for generating Go code from proto/.
# Build:  docker build -t birdman-protogen -f protogen.Dockerfile .
# Run:    docker run --rm -v "$PWD":/workspace birdman-protogen generate
FROM golang:1.24
COPY --from=bufbuild/buf:1.55.1 /usr/local/bin/buf /usr/local/bin/buf
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6 \
 && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
WORKDIR /workspace
ENTRYPOINT ["buf"]
