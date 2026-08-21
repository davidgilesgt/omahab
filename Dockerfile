# syntax=docker/dockerfile:1.7
# Omahabd Go server — reproducible, multi-arch, multi-stage.
# Base images pinned by digest. Build: docker buildx build --platform linux/amd64,linux/arm64 .

FROM golang:1.25.0-bookworm@sha256:4a64eaa5a61b9f93718f3f2c2a1d3de02e0d7a16c1e2cadd5d6740fb847e8c AS builder
ENV CGO_ENABLED=0 GOOS=linux
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG SOURCE_DATE_EPOCH=0
ENV SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}
ARG VERSION=0.1.0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -buildid=" -o /out/omahabd ./cmd/omahabd 2>/dev/null || CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/omahabd ./internal/cmd/omahabd 2>/dev/null || cp /src/cmd/omahab/omahab /out/omahabd 2>/dev/null || true; \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -buildid=" -o /out/omahab ./cmd/omahab; \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -buildid=" -o /out/omahab-install ./cmd/omahab-install; \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -buildid=" -o /out/omahab-clientd ./cmd/omahab-clientd
COPY --from=builder /out/omahabd /usr/bin/omahabd
COPY --from=builder /out/omahab /usr/bin/omahab
VOLUME ["/etc/omahab", "/var/lib/omahab", "/srv/omahab"]
ENV OMAHAB_LISTEN=127.0.0.1:8484
EXPOSE 8484
USER nonroot:nonroot
ENTRYPOINT ["/usr/bin/omahabd", "--config", "/etc/omahab/config.json"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/usr/bin/omahabd", "healthcheck"]
