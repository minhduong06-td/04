FROM golang:1.22-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /app/api ./cmd/api
RUN CGO_ENABLED=0 go build -o /app/mock-worker ./cmd/mock-worker

FROM alpine:3.19

RUN adduser -u 1001 -D -H hustack && \
    mkdir -p /var/lib/hustack/sources && \
    chown -R hustack:hustack /var/lib/hustack

COPY --from=builder /app/api /app/api
COPY --from=builder /app/mock-worker /app/mock-worker
COPY --from=builder /src/web /app/web
COPY --from=builder /src/migrations /app/migrations

RUN chmod 755 /app/api /app/mock-worker

USER hustack:hustack
WORKDIR /app

EXPOSE 8080
