FROM --platform=$BUILDPLATFORM golang:1.26.2-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY web ./web
COPY config.defaults.toml ./

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/grok2api-go ./cmd/server

FROM debian:bookworm-slim

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/grok2api-go /app/grok2api-go
COPY web /app/web
COPY config.defaults.toml /app/config.defaults.toml
COPY .env.example /app/.env.example

ENV APP_BASE_DIR=/app
ENV DATA_DIR=/app/data
ENV LOG_DIR=/app/logs
ENV SERVER_HOST=0.0.0.0
ENV SERVER_PORT=8000

EXPOSE 8000

VOLUME ["/app/data", "/app/logs"]

ENTRYPOINT ["/app/grok2api-go"]
