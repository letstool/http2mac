#!/bin/bash

go build \
    -trimpath \
    -ldflags="-extldflags -static -s -w" \
    -o ./out/http2mac ./cmd/http2mac
