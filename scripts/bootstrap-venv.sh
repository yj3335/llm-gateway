#!/usr/bin/env bash
# Builds the self-contained project toolchain inside .venv (no system installs):
#   - Python venv (tooling: PyYAML for manifest validation)
#   - Go toolchain           -> .venv/go
#   - PostgreSQL 16 + pgvector (prebuilt binaries from the pgserver wheel) -> .venv/opt/pgserver
#   - Redis 7.4 built from source -> .venv/opt/redis
#   - k6 load-testing binary -> .venv/bin/k6
#   - npm cache + Playwright browsers are redirected into .venv
# Usage: scripts/bootstrap-venv.sh   then   source .venv/bin/activate
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VENV="$ROOT/.venv"
GO_VERSION="${GO_VERSION:-go1.27.2}"
REDIS_VERSION="${REDIS_VERSION:-7.4.2}"
K6_VERSION="${K6_VERSION:-v2.3.0}"
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"   # darwin | linux
ARCH="$(uname -m)"; [ "$ARCH" = "x86_64" ] && ARCH=amd64; [ "$ARCH" = "aarch64" ] && ARCH=arm64

[ -d "$VENV" ] || python3 -m venv "$VENV"
"$VENV/bin/pip" install -q --upgrade pip pyyaml

if [ ! -x "$VENV/go/bin/go" ]; then
  echo "==> Go $GO_VERSION"
  curl -sSL "https://go.dev/dl/$GO_VERSION.$OS-$ARCH.tar.gz" | tar -xz -C "$VENV"
fi

if [ ! -x "$VENV/opt/pgserver/pginstall/bin/postgres" ]; then
  echo "==> PostgreSQL 16 + pgvector (pgserver wheel binaries)"
  TMP="$(mktemp -d)"
  "$VENV/bin/pip" download -q pgserver==0.1.4 --no-deps --only-binary=:all: --python-version 3.12 -d "$TMP"
  mkdir -p "$VENV/opt" && (cd "$TMP" && unzip -q pgserver-*.whl && mv pgserver "$VENV/opt/pgserver")
  chmod +x "$VENV/opt/pgserver/pginstall/bin/"*
  rm -rf "$TMP"
fi

if [ ! -x "$VENV/opt/redis/bin/redis-server" ]; then
  echo "==> Redis $REDIS_VERSION (from source)"
  TMP="$(mktemp -d)"
  curl -sSL "https://download.redis.io/releases/redis-$REDIS_VERSION.tar.gz" | tar -xz -C "$TMP"
  make -s -C "$TMP/redis-$REDIS_VERSION" -j"$(getconf _NPROCESSORS_ONLN)" BUILD_TLS=no MALLOC=libc >/dev/null
  make -s -C "$TMP/redis-$REDIS_VERSION" PREFIX="$VENV/opt/redis" install >/dev/null
  rm -rf "$TMP"
fi

if [ ! -x "$VENV/bin/k6" ]; then
  echo "==> k6 $K6_VERSION"
  TMP="$(mktemp -d)"
  if [ "$OS" = "darwin" ]; then
    curl -sSL "https://github.com/grafana/k6/releases/download/$K6_VERSION/k6-$K6_VERSION-macos-$ARCH.zip" -o "$TMP/k6.zip"
    (cd "$TMP" && unzip -q k6.zip && cp k6-*/k6 "$VENV/bin/k6")
  else
    curl -sSL "https://github.com/grafana/k6/releases/download/$K6_VERSION/k6-$K6_VERSION-linux-$ARCH.tar.gz" | tar -xz -C "$TMP"
    cp "$TMP"/k6-*/k6 "$VENV/bin/k6"
  fi
  rm -rf "$TMP"
fi

if ! grep -q "llm-gateway self-contained toolchain" "$VENV/bin/activate"; then
  cat >> "$VENV/bin/activate" <<'ACT'

# --- llm-gateway self-contained toolchain (Go, Postgres+pgvector, Redis) ---
export LLMGW_VENV="$VIRTUAL_ENV"
export GOROOT="$VIRTUAL_ENV/go"
export GOPATH="$VIRTUAL_ENV/gopath"
export GOMODCACHE="$VIRTUAL_ENV/gopath/pkg/mod"
export GOCACHE="$VIRTUAL_ENV/gocache"
export GOTOOLCHAIN=local
export PLAYWRIGHT_BROWSERS_PATH="$VIRTUAL_ENV/ms-playwright"
export npm_config_cache="$VIRTUAL_ENV/npm-cache"
export PATH="$VIRTUAL_ENV/go/bin:$VIRTUAL_ENV/gopath/bin:$VIRTUAL_ENV/opt/pgserver/pginstall/bin:$VIRTUAL_ENV/opt/redis/bin:$PATH"
ACT
fi
echo "==> done. Run: source .venv/bin/activate"
