FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags='-w -s' -o tsserve .

FROM alpine:latest
# Pin a floor on ca-certificates: releases before 20261020-r0 still trust eMudhra
# intermediate CAs whose private keys are compromised. The constraint makes
# the build fail if the package repository cannot supply a fixed version. Drop the pin once alpine:latest ships it.
RUN apk add --no-cache 'ca-certificates>=20261020-r0'
COPY --from=builder /build/tsserve /usr/local/bin/tsserve
ENTRYPOINT ["tsserve"]
