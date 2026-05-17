FROM golang:alpine AS builder

ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=${VERSION}" -o /sentinel ./cmd/sentinel/

FROM alpine:latest

RUN apk add --no-cache ca-certificates git
COPY --from=builder /sentinel /sentinel
COPY test/integration/entrypoint.sh /entrypoint.sh

ARG REGISTRY_URL=https://github.com/ny4rl4th0t3p/chain-registry
ENV REGISTRY_URL=${REGISTRY_URL}

ENTRYPOINT ["/entrypoint.sh"]