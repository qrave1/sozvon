# этап сборки
FROM golang:1.26-alpine3.24 AS builder

WORKDIR /app

COPY go.mod go.sum ./
COPY vendor ./vendor

RUN CGO_ENABLED=0 go build -mod=vendor -o build ./main.go

# финальный образ
FROM alpine:3.24

WORKDIR /app

# Копируем бинарник
COPY --from=builder /app/build .
COPY --from=builder /app/web ./web
COPY --from=builder /app/.env.example ./.env.example

CMD ["./build"]
