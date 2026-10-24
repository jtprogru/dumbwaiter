# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/dumbwaiter ./cmd/dumbwaiter

# static + CA certificates for HTTPS to S3, runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dumbwaiter /dumbwaiter
USER nonroot:nonroot
EXPOSE 8080/tcp 8443/tcp 8443/udp 9090/tcp
ENTRYPOINT ["/dumbwaiter"]
CMD ["-config", "/etc/dumbwaiter/config.yaml"]
