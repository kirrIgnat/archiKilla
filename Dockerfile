FROM golang:1.26.9-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/server ./cmd/server && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/bot ./cmd/bot
FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata && addgroup -S lms && adduser -S -G lms lms
WORKDIR /app
COPY --from=builder /out/ /app/
USER lms
EXPOSE 8083
ENTRYPOINT ["/app/server"]
