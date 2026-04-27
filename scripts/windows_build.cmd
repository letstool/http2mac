@echo off
go build ^
    -trimpath ^
    -ldflags="-s -w" ^
    -o .\out\http2mac.exe .\cmd\http2mac
