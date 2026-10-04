# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/emby-prober ./cmd/emby-prober
RUN mkdir -p /out/state && chown 65532:65532 /out/state

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/zhousiru/emby-prober"
LABEL org.opencontainers.image.description="Select a Mihomo exit using measured Emby video throughput"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/emby-prober /usr/local/bin/emby-prober
COPY --from=build --chown=65532:65532 /out/state /state
USER 65532:65532
WORKDIR /state
ENTRYPOINT ["/usr/local/bin/emby-prober"]
CMD ["--config", "/config/config.json"]
