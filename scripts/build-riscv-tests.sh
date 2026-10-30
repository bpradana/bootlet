#!/usr/bin/env bash
# Builds the official riscv-tests (physical-memory "p" environment) with zig cc,
# so no RISC-V GNU toolchain is needed. Output: third_party/riscv-tests-build/<suite>/<test>.
set -euo pipefail
cd "$(dirname "$0")/.."

REV=3df703cd52d5f0d8224e3c50bf03c6cb70efff98
SRC=third_party/riscv-tests
OUT=third_party/riscv-tests-build

if [ ! -d "$SRC/.git" ]; then
  git clone -q https://github.com/riscv-software-src/riscv-tests.git "$SRC"
fi
git -C "$SRC" fetch -q origin "$REV" 2>/dev/null || true
git -C "$SRC" checkout -q "$REV"
git -C "$SRC" submodule update -q --init env

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
# LLVM's assembler rejects a weak symbol later declared .global (the env
# declares the trap handlers .weak); the definition in the test wins either
# way. Patch a mirror of all suites, since tests include each other.
SUITES="rv64ui rv64um rv64ua rv64uf rv64ud rv64uc rv64mi rv64si"
for suite in $SUITES; do
  mkdir -p "$TMP/$suite"
  for src in "$SRC/isa/$suite"/*.S; do
    sed -E '/^[[:space:]]*\.global[[:space:]]+(m|s)tvec_handler/d' "$src" > "$TMP/$suite/$(basename "$src")"
  done
done
count=0
for suite in $SUITES; do
  mkdir -p "$OUT/$suite"
  for src in "$TMP/$suite"/*.S; do
    zig cc -target riscv64-freestanding-none -mcpu=baseline_rv64 -nostdlib -static \
      -T "$SRC/env/p/link.ld" -I"$SRC/env/p" -I"$SRC/isa/macros/scalar" -I"$TMP/$suite" \
      "$src" -o "$OUT/$suite/$(basename "$src" .S)"
    count=$((count + 1))
  done
done
echo "built $count tests into $OUT"
