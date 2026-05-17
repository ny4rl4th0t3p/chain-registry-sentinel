FROM golang:alpine AS builder

ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=${VERSION}" -o /sentinel ./cmd/sentinel/

FROM alpine:latest

RUN apk add --no-cache ca-certificates
COPY --from=builder /sentinel /sentinel
ENTRYPOINT ["/sentinel"]