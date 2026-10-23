#!/bin/sh
# Run on a ZimaOS host as root after `zpkg install restic.raw`.
#   sudo sh host-test.sh           job "hosttest": repository, run, timer, snapshot, restore, missing folder
#   sudo sh host-test.sh cleanup   remove everything this test created
# Test names: job /DATA/AppData/restic/jobs/hosttest, repository /DATA/AppData/restic-hosttest-repo,
# source /DATA/AppData/restic-hosttest-src, restores under /DATA/restic-restore/hosttest-*.
set -u
B=/DATA/AppData/restic; J=$B/jobs/hosttest; R=/DATA/AppData/restic-hosttest-repo; SRC=/DATA/AppData/restic-hosttest-src
ok=0; bad=0
pass() { ok=$((ok + 1)); echo "OK   $1"; }
fail() { bad=$((bad + 1)); echo "FAIL $1"; }

if [ "${1:-}" = cleanup ]; then
  restic-zima remove hosttest
  rm -rf "$J" "$R" "$SRC" /DATA/restic-restore/hosttest-* /DATA/AppData/restic-tiletest-repo* /DATA/restic-restore/tiletest-* "$B"/jobs/.deleted/tiletest-*
  rmdir "$B/jobs/.deleted" 2>/dev/null
  rmdir /DATA/restic-restore 2>/dev/null
  echo "cleanup done: $(ls -d "$J" "$R" "$SRC" /etc/systemd/system/restic-zima-hosttest.* 2>/dev/null | wc -l) test paths left"
  exit 0
fi

[ -e "$J" ] && { echo "refusing: $J exists (run cleanup first)"; exit 1; }
v=$(restic version 2>&1); case "$v" in "restic 0.19.1 "*) pass "restic: $v" ;; *) fail "restic version: $v" ;; esac
a=$(systemctl is-active restic-web.service); [ "$a" = active ] && pass "restic-web active" || fail "restic-web: $a"

umask 077
mkdir -p "$J" "$SRC/sub"
echo "eins" > "$SRC/a.txt"; echo "zwei" > "$SRC/sub/b.txt"
echo "$R" > "$J/repository"; head -c 24 /dev/urandom | base64 > "$J/password"
echo "$SRC" > "$J/paths"; : > "$J/excludes"; echo "04:30" > "$J/schedule"
restic-zima restic hosttest init >/dev/null 2>&1 && pass "repository created" || fail "restic init"

out=$(restic-zima units hosttest 2>&1); echo "  $out"
[ "$(systemctl is-enabled restic-zima-hosttest.timer 2>&1)" = enabled ] && pass "timer enabled" || fail "timer not enabled"
next=$(systemctl show -p NextElapseUSecRealtime --value restic-zima-hosttest.timer)
case "$next" in *04:30:00*) pass "next run: $next" ;; *) fail "next run: '$next'" ;; esac

# The job through systemd, as the timer starts it (also proves After=/RequiresMountsFor= resolve).
systemctl start restic-zima-hosttest.service; rc=$?
res=$(systemctl show -p Result --value restic-zima-hosttest.service)
[ "$rc" -eq 0 ] && [ "$res" = success ] && pass "backup via systemd (Result=$res)" || fail "backup via systemd rc=$rc Result=$res: $(tail -n 2 "$J/last.log")"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); s=d["summary"]; assert d["rc"]==0 and s["files_new"]==2, d; print("  last.json: rc", d["rc"], "files_new", s["files_new"])' "$J/last.json" \
  && pass "last.json valid, 2 new files" || fail "last.json: $(cat "$J/last.json")"

id=$(restic-zima restic hosttest snapshots --json | python3 -c 'import json,sys; s=json.load(sys.stdin); print(s[-1]["id"] if len(s)==1 else "count=%d" % len(s))')
case "$id" in *[!a-f0-9]*|"") fail "snapshots: '$id'" ;; *) [ "${#id}" -eq 64 ] && pass "one snapshot ${id%"${id#????????}"}" || fail "snapshot id: $id" ;; esac

T=/DATA/restic-restore/hosttest-by-test
restic-zima restore hosttest "$id" "$T" && [ "$(cat "$T$SRC/sub/b.txt")" = zwei ] && pass "restore into $T" || fail "restore: $(tail -n 2 "$J/restore.log")"
o=$(restic-zima restore hosttest "$id" "$T" 2>&1); rc=$?
case "$rc:$o" in 1:*"restore only into a new directory"*) pass "restore refuses an existing folder" ;; *) fail "restore over an existing folder: rc=$rc $o" ;; esac

# An unplugged disk: restic alone exits 0 (measured 0.19.1), restic-zima must report it.
echo "/media/restic-hosttest-gone" >> "$J/paths"
restic-zima run hosttest >/dev/null 2>&1; rc=$?
m=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["missing_paths"])' "$J/last.json")
[ "$rc" -eq 3 ] && [ "$m" = 1 ] && pass "missing folder: exit 3, missing_paths=1" || fail "missing folder: rc=$rc missing_paths=$m"

echo "result: $ok OK, $bad FAIL"
[ "$bad" -eq 0 ]
