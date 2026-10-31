# Boot status lines in the style of systemd. Every line also goes to
# /dev/.boot.log (devtmpfs survives the switch to the persistent root), which
# rcS copies to /var/log/boot.log after the screen is cleared.
BOOTLOG=/dev/.boot.log

_line() { # colour, label, text
  printf '[\033[%sm%s\033[0m] %s\n' "$1" "$2" "$3" >/dev/console
  printf '[%s] %s\n' "$2" "$3" >>"$BOOTLOG" 2>/dev/null
}
ok() { _line 32 "  OK  " "$*"; }
fail() { _line 31 "FAILED" "$*"; }
info() {
  printf '         %s\n' "$*" >/dev/console
  printf '         %s\n' "$*" >>"$BOOTLOG" 2>/dev/null
}

# step "Description" command...: runs the command and reports OK or FAILED.
step() {
  desc=$1
  shift
  if "$@" >/dev/null 2>&1; then ok "$desc"; else fail "$desc"; fi
}
