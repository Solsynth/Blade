FROM golang:1.23-alpine AS builder

WORKDIR /app

RUN apk add --no-cache gcc musl-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux go build -o gateway ./cmd/main.go

FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/gateway .
COPY --from=builder /app/configs ./configs

EXPOSE 6000

ENV GIN_MODE=release
ENV ZEROLOG_PRETTY=true

ENTRYPOINT ["./gateway"]
