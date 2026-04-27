@echo off
cd /d "%~dp0.."
if not exist db mkdir db

REM Mode 1 (default): fetch CSV from CDN (set LICENSE_KEY for licensed access)
REM Mode 2 (peer):    set MAC_DB_URL=http://upstream-host:8080

set LISTEN_ADDR=127.0.0.1:8080
set MAC_DB_DIR=.\db
set MAC_MAX_IPS=100
REM set LICENSE_KEY=your_token_here
.\out\http2mac.exe
