# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/qw2api ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates tzdata \
 && adduser -D -u 10001 app \
 && mkdir -p /app/pats /app/data \
 && chown -R app:app /app
USER app
WORKDIR /app
COPY --from=build /out/qw2api /app/qw2api
COPY config.json /app/config.json
EXPOSE 7864
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8963/healthz || exit 1
ENTRYPOINT ["/app/qw2api", "-config", "/app/config.json"]
