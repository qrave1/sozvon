# этап сборки
FROM golang:1.25.0-alpine3.21 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -o build ./main.go

# финальный образ
FROM alpine:3.21

WORKDIR /app

# Копируем бинарник
COPY --from=builder /app/build .
COPY --from=builder /app/web ./web

CMD ["./build"]