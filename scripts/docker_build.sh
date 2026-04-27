#!/bin/bash

IMAGE_TAG=letstool/http2mac:latest

docker build \
        -t "$IMAGE_TAG" \
       -f build/Dockerfile \
       .
