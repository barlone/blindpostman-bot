rem setlocal: without it CGO_ENABLED=0 stayed set in the calling console and every
rem later go build there, of anything, went without cgo.
setlocal

rem CGO off, as in 3_build-app.sh: no C compiler needed.
set CGO_ENABLED=0

go build -ldflags="-s -w" -o blindpostman-bot
