FROM golang:1.24.0-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o app

FROM alpine:latest
WORKDIR /app

RUN apk add --no-cache ca-certificates openssh

COPY --from=builder /app/app .
COPY content ./content

EXPOSE 22

CMD ["./app"]
