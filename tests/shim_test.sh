#!/bin/sh
# Tests for the plugin's shell shims (plugin/bin/docket-hook, plugin/bin/docket).
set -u
root=$(cd "$(dirname "$0")/.." && pwd)
hookshim="$root/plugin/bin/docket-hook"
clishim="$root/plugin/bin/docket"
fails=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; fails=$((fails + 1)); }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/home"

# 1. No binary installed: the raw payload is kept as one valid JSON line.
out=$(printf '{\n "session_id": "s1",\n "last_assistant_message": "line1\\nline2"\n}\n' |
	HOME="$tmp/home" DOCKET_BIN= DOCKET_HOME="$tmp/dk" PATH=/usr/bin:/bin sh "$hookshim" Stop)
code=$?
seg=$(ls "$tmp/dk/spool/"seg-*.jsonl 2>/dev/null | head -1)
if [ $code -eq 0 ] && [ -z "$out" ] && [ -n "$seg" ] && [ "$(wc -l <"$seg" | tr -d ' ')" = 1 ] &&
	python3 -c 'import json,sys; r=json.loads(open(sys.argv[1]).read()); assert r["event"]=="Stop" and r["raw"]["session_id"]=="s1" and r["v"]==0' "$seg"; then
	pass "fallback keeps the raw payload as one JSON line"
else
	fail "fallback (exit=$code out=$out seg=$seg)"
fi

# 2. Empty stdin still produces valid JSON.
rm -rf "$tmp/dk"
: | HOME="$tmp/home" DOCKET_BIN= DOCKET_HOME="$tmp/dk" PATH=/usr/bin:/bin sh "$hookshim" SessionEnd
seg=$(ls "$tmp/dk/spool/"seg-*.jsonl 2>/dev/null | head -1)
if [ -n "$seg" ] && python3 -c 'import json,sys; assert json.loads(open(sys.argv[1]).read())["raw"] is None' "$seg"; then
	pass "empty payload becomes raw:null"
else
	fail "empty payload"
fi

# 3. An unwritable spool never fails the hook.
touch "$tmp/notadir"
echo '{}' | HOME="$tmp/home" DOCKET_BIN= DOCKET_HOME="$tmp/notadir" PATH=/usr/bin:/bin sh "$hookshim" Stop
[ $? -eq 0 ] && pass "unwritable spool still exits 0" || fail "unwritable spool exit code"

# 4. With a binary, the hook shim passes the event and stdin through, and
#    exits 0 even if the binary fails.
cat >"$tmp/fake" <<'EOF'
#!/bin/sh
echo "$@" >"$FAKE_OUT.args"
cat >"$FAKE_OUT.stdin"
exit 3
EOF
chmod +x "$tmp/fake"
echo '{"x":1}' | FAKE_OUT="$tmp/call" DOCKET_BIN="$tmp/fake" HOME="$tmp/home" sh "$hookshim" Notification
code=$?
if [ $code -eq 0 ] && [ "$(cat "$tmp/call.args")" = "hook Notification" ] && [ "$(cat "$tmp/call.stdin")" = '{"x":1}' ]; then
	pass "hook shim forwards to the binary and masks its exit code"
else
	fail "hook shim forwarding (exit=$code)"
fi

# 4b. The waiter's exit code (2 = wake the session) passes through.
printf '#!/bin/sh\ncat >/dev/null\nexit 2\n' >"$tmp/fake3"
chmod +x "$tmp/fake3"
echo '{}' | DOCKET_BIN="$tmp/fake3" HOME="$tmp/home" sh "$hookshim" Wait
[ $? -eq 2 ] && pass "waiter exit code passes through" || fail "waiter exit code masked"

# 5. The CLI shim passes arguments through and reports a missing binary.
printf '#!/bin/sh\necho "args:$*"\n' >"$tmp/fake2"
chmod +x "$tmp/fake2"
got=$(DOCKET_BIN="$tmp/fake2" HOME="$tmp/home" sh "$clishim" ask "Push?" --blocking)
[ "$got" = "args:ask Push? --blocking" ] && pass "cli shim forwards arguments" || fail "cli shim forwarding: $got"
if ! DOCKET_BIN= HOME="$tmp/home" PATH=/usr/bin:/bin sh "$clishim" ask x 2>"$tmp/err"; then
	grep -q "isn't installed" "$tmp/err" && pass "cli shim explains a missing binary" || fail "cli shim message"
else
	fail "cli shim should fail without a binary"
fi

[ $fails -eq 0 ] && echo "all shim tests passed" || { echo "$fails shim test(s) failed"; exit 1; }
