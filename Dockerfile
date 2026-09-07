FROM golang:1.24-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=1 go build -o /max-telegram-bridge-bot .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

COPY Russian_Trusted_Root_CA.cer /usr/local/share/ca-certificates/russian-trusted-ca.crt
COPY Russian_Trusted_Sub_CA.cer /usr/local/share/ca-certificates/russian-trusted-sub-ca.crt
COPY Russian_Trusted_Sub_CA_2024.cer /usr/local/share/ca-certificates/russian-trusted-sub-ca-2024.crt
RUN update-ca-certificates
RUN adduser -D -h /app bridge
USER bridge
WORKDIR /app

COPY --from=builder /max-telegram-bridge-bot /usr/local/bin/max-telegram-bridge-bot

ENTRYPOINT ["max-telegram-bridge-bot"]
