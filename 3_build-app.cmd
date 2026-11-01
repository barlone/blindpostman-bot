rem setlocal: without it CGO_ENABLED=0 stayed set in the calling console and every
rem later go build there, of anything, went without cgo.
setlocal

rem CGO off, as in 3_build-app.sh: no C compiler needed.
set CGO_ENABLED=0

rem Target is the LXC container: Linux x86-64.
set GOOS=linux
set GOARCH=amd64

go build -ldflags="-s -w" -o blindpostman-bot ./cmd/blind-postman
