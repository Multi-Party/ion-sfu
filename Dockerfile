FROM golang:stretch

WORKDIR /app

COPY go.mod go.sum /app/
RUN cd /app && go mod download

COPY config.toml /app/config.toml
COPY pkg/ /app/pkg
COPY cmd/ /app/cmd

RUN cd /app && go build -o sfu cmd/signal/json-rpc/main.go
