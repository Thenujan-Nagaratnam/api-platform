FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY go.mod .
COPY main.go .

RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -o mock-backend main.go

FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

COPY --from=builder /app/mock-backend .

EXPOSE 8081

CMD ["./mock-backend"]
