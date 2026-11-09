#!/bin/sh
# Plain assertions. Each prints PASS or FAIL; exit status counts failures.
# Expected values come from the test launcher. Never print environment values.
fails=0
ok() { echo "PASS $1"; }
bad() { echo "FAIL $1: $2"; fails=$((fails + 1)); }
check() { name=$1; shift; if "$@" >/dev/null 2>&1; then ok "$name"; else bad "$name" "command failed"; fi; }
refuse() { name=$1; shift; if "$@" >/dev/null 2>&1; then bad "$name" "unexpectedly succeeded"; else ok "$name"; fi; }
same() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "does not match"; fi; }

same uid "$(id -u)" "$EXPECT_UID"
same no-capabilities "$(awk '/^CapEff:/ {print $2}' /proc/self/status)" "0000000000000000"
same seccomp "$(awk '/^Seccomp:/ {print $2}' /proc/self/status)" "$EXPECT_SECCOMP"
same no-new-privileges "$(awk '/^NoNewPrivs:/ {print $2}' /proc/self/status)" "1"

same root-entries "$(ls -A / | tr '\n' ' ')" "$EXPECT_ROOT"
same etc-entries "$(ls -A /etc | tr '\n' ' ')" "$EXPECT_ETC"
same home-entries "$(ls -A /home | tr '\n' ' ')" "agent "
same home-empty "$(ls -A "$HOME" | wc -l)" "0"
refuse no-host-home test -e "$HOST_HOME"
check work-writable sh -c 'touch /work/.probe && rm /work/.probe'
check out-writable sh -c 'touch /out/.probe && rm /out/.probe'
check store-readable test -r /ro/store.sqlite
refuse store-read-only sh -c ': >> /ro/store.sqlite'
check clone-readable git -C /ro/clone log -1 --format=%H
refuse clone-read-only touch /ro/clone/.probe
refuse usr-read-only touch /usr/.probe

# Only names are inspected or printed, including on failure.
if python3 -I -c '
import os, sys
expected = set(os.environ["EXPECT_ENV"].split())
actual = set(os.environ) - {"PWD", "SHLVL", "_", "OLDPWD"}
for name in sorted(actual - expected):
    print("unexpected variable " + name)
for name in sorted(expected - actual):
    print("missing variable " + name)
sys.exit(actual != expected)
' > /tmp/env-names; then
  ok env-names
else
  bad env-names "$(cat /tmp/env-names)"
fi

same pid1 "$(cat /proc/1/comm)" "bwrap"
nprocs=$(ls /proc | grep -c '^[0-9][0-9]*$')
if [ "$nprocs" -le 6 ]; then ok few-processes; else bad few-processes "too many processes visible"; fi

check relay-socket test -S /run/relay.sock
refuse nested-userns unshare -U true
if [ -n "${EXPECT_NO_VSOCK:-}" ]; then
  refuse vsock python3 -I -c 'import socket; socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)'
fi

same interfaces "$(awk -F: 'NR>2 {gsub(/ /, "", $1); print $1}' /proc/net/dev | tr '\n' ' ')" "lo "
# The relay command starts this script only once socat listens: one request, no retry.
code=$(curl --noproxy '*' -s -o /dev/null -w '%{http_code}' --max-time 2 http://127.0.0.1:8787/route-probe)
same route-answers "$code" "503"
refuse other-loopback-port curl --noproxy '*' -s --max-time 2 "http://127.0.0.1:$OTHER_HOST_PORT/"
# A second host-only loopback address stands for an address outside this namespace.
# This test never sends a packet to an external network.
refuse outside-address curl --noproxy '*' -s --max-time 2 "http://127.0.0.2:$OUTSIDE_HOST_PORT/"
refuse dns getent hosts sandbox-check.invalid

echo "failures=$fails"
exit "$fails"
