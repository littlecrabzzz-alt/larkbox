FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.1.0-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags="-s -w -X github.com/littlecrabzzz-alt/larkbox/internal/server.Version=$VERSION" \
    -o /out/larkbox ./cmd/larkbox && mkdir -p /out/data

FROM alpine:3.23
ARG VERSION=0.1.0-dev
LABEL org.opencontainers.image.source="https://github.com/littlecrabzzz-alt/larkbox" \
      org.opencontainers.image.version="$VERSION"
COPY --from=build /out/larkbox /usr/local/bin/larkbox
COPY --from=build --chown=65532:65532 /out/data /data
ENV LARKBOX_ADDR=0.0.0.0:8080
ENV LARKBOX_JSON_LOGS=true
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=2s --timeout=2s --retries=10 CMD ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/healthz"]
ENTRYPOINT ["/usr/local/bin/larkbox"]
