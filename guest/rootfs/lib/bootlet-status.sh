# Boot status lines in the style of systemd: [  OK  ] / [FAILED].
ok() { printf '[  \033[32mOK\033[0m  ] %s\n' "$*" >/dev/console; }
fail() { printf '[\033[31mFAILED\033[0m] %s\n' "$*" >/dev/console; }
info() { printf '         %s\n' "$*" >/dev/console; }
