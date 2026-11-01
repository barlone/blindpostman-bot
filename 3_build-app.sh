#!/bin/bash

set -euo pipefail

# Run from anywhere, this acts on this checkout: from another directory it used to
# work on whatever repository (or module) was there — and the pull/push scripts, whose
# failed "source gittoken" did not stop them, pushed that repository here.
cd "$(dirname "$(readlink -f "$0")")"

# CGO off: nothing in the module needs it (lib/pq is pure Go). With it on, the build
# needed gcc on the host and produced a binary tied to that host's glibc, so one built
# on a newer system failed to start on an older one.
CGO_ENABLED=0 go build -ldflags="-s -w" -o blindpostman-bot
