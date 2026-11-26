FROM golang:1.22-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/babel ./babel-go

FROM alpine:3.20

RUN apk add --no-cache ca-certificates \
    && addgroup -S babel \
    && adduser -S -G babel -h /data babel \
    && mkdir -p /data \
    && chown babel:babel /data

COPY --from=builder /out/babel /usr/local/bin/babel

USER babel
WORKDIR /data
ENV HOME=/data

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/babel", "-serve", "127.0.0.1:8080"]
