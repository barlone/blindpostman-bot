#!/bin/bash

set -euo pipefail

# Run from anywhere, this acts on this checkout: from another directory it used to
# work on whatever repository (or module) was there — and the pull/push scripts, whose
# failed "source gittoken" did not stop them, pushed that repository here.
cd "$(dirname "$(readlink -f "$0")")"

set -o allexport
# ./ — a bare name is looked up along PATH first.
source ./gittoken
set +o allexport

git pull  https://barlone:${GITTOKEN}@github.com/barlone/blindpostman-bot.git
       