#!/bin/sh
# Double-click to open suspt. It builds itself the first time (needs Go: https://go.dev/dl/),
# then opens the app in your browser. Close this window to stop the app.
cd "$(dirname "$0")" || exit 1
if [ ! -x ./suspt ] || [ -n "$(find . \( -name '*.go' -o -path './web/*' \) -newer ./suspt | head -1)" ]; then
  if ! command -v go >/dev/null 2>&1; then
    echo "suspt needs Go to build the first time. Install it from https://go.dev/dl/ and double-click again."
    read -r _
    exit 1
  fi
  echo "Building suspt (first run or after an update)..."
  # Build to a temp file and swap it in, so a copy that is already running keeps working.
  go build -o suspt.tmp . && mv -f suspt.tmp suspt || { echo "Build failed; see above."; read -r _; exit 1; }
fi
exec ./suspt
