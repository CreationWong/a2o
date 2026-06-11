# ---- Build Stage ----
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY main.go .

# CGO_ENABLED=0 生成纯静态二进制，兼容 scratch / alpine
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o a2o main.go

# ---- Runtime Stage ----
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /app/a2o .

EXPOSE 9999

ENTRYPOINT ["/app/a2o"]
