#!/usr/bin/env bash
# Rebuild Mark's hardened grepai fork and install to ~/.local/bin/grepai
# (PATH already prefers ~/.local/bin over Homebrew).
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

SRC="${GREPAI_SRC:-$HOME/Code/Labs/grepai-src}"
VERSION="${GREPAI_VERSION:-0.35.0-mark.4}"
DEST="${GREPAI_DEST:-$HOME/.local/bin/grepai}"

cd "$SRC"
mkdir -p bin
go test ./store/ -count=1 -timeout 60s
go build -ldflags "-s -w -X main.version=${VERSION}" -o "bin/grepai" ./cmd/grepai
mkdir -p "$(dirname "$DEST")"
cp -f bin/grepai "$DEST"
chmod +x "$DEST"
echo "installed: $DEST ($("$DEST" version))"
if command -v brew >/dev/null 2>&1; then
  brew_grepai="$(brew --prefix 2>/dev/null)/bin/grepai"
  if [ -x "$brew_grepai" ] && [ "$brew_grepai" != "$DEST" ]; then
    echo "homebrew still at: $brew_grepai (PATH should prefer ~/.local/bin)"
  fi
fi
