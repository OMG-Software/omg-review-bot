#!/bin/bash
# Tests packaging/publish-latest.sh against mock_gh.py, a fake gh backed by a
# JSON file, including failures part way through staging and swapping.
#   packaging/test/publish-latest_test.sh
here=$(cd "$(dirname "$0")" && pwd)
script=$here/../publish-latest.sh
t=$(mktemp -d)
mkdir -p "$t/bin"
printf '#!/bin/sh\nexec python3 %s "$@"\n' "$here/mock_gh.py" >"$t/bin/gh"
chmod +x "$t/bin/gh"
export PATH="$t/bin:$PATH" MOCK_STATE="$t/state.json" GH_REPO=o/r GH_TOKEN=x VERSION=0.1.0
OLD=1111111111111111111111111111111111111111
NEW=2222222222222222222222222222222222222222
fails=0

build() { # build DIR TAG FILE... : files whose content names the build
    rm -rf "$1"; mkdir -p "$1"
    local d=$1 b=$2; shift 2
    for f in "$@"; do echo "$b $f" >"$d/$f"; done
}
echo notes >"$t/notes.md"

# A release published by run 41 at OLD, with an asset the next build drops.
published() {
    build "$t/old" run41 a.rpm b.deb SHA256SUMS stale.tar.gz
    python3 - "$t" "$OLD" <<'EOF'
import hashlib, json, os, sys
t, old = sys.argv[1:]
assets = []
for i, n in enumerate(sorted(os.listdir(f"{t}/old"))):
    d = open(f"{t}/old/{n}", "rb").read()
    assets.append({"id": 100 + i, "name": n, "size": len(d), "state": "uploaded", "sha": hashlib.sha256(d).hexdigest()})
json.dump({"tag": old, "tag_original": old, "compare": {}, "fail": {}, "counts": {}, "next_id": 500,
           "release": {"id": 900, "name": "Latest build (0.1.0-41)",
                       "body": f"old notes\n<!-- prbot-build run=41 sha={old} -->\n", "assets": assets}},
          open(f"{t}/state.json", "w"))
EOF
    cp "$t/state.json" "$t/before.json"
}

set_state() { python3 -c "import json,sys;p='$MOCK_STATE';s=json.load(open(p));exec(sys.argv[1]);json.dump(s,open(p,'w'))" "$1"; }

run() { # run SHA RUN_NUMBER
    GITHUB_SHA=$1 GITHUB_RUN_NUMBER=$2 GITHUB_RUN_ID=77$2 GITHUB_RUN_ATTEMPT=1 \
        bash "$script" "$t/new" "$t/notes.md" >"$t/out.log" 2>&1
}

check() { # check NAME CONDITION...
    local name=$1; shift
    if "$@"; then echo "ok   $name"; else echo "FAIL $name"; sed 's/^/     /' "$t/out.log"; fails=$((fails + 1)); fi
}

unchanged() { python3 -c "import json;a=json.load(open('$t/before.json'));b=json.load(open('$MOCK_STATE'));exit(0 if (a['tag'],a['release'])==(b['tag'],b['release']) else 1)"; }

# Release holds exactly the new build: same names and contents, tag, title and marker.
is_new() { # is_new SHA RUN
    python3 - "$t" "$1" "$2" <<'EOF'
import hashlib, json, os, sys
t, sha, run = sys.argv[1:]
s = json.load(open(f"{t}/state.json"))
want = {n: hashlib.sha256(open(f"{t}/new/{n}", "rb").read()).hexdigest() for n in os.listdir(f"{t}/new")}
have = {a["name"]: a["sha"] for a in s["release"]["assets"]}
ok = have == want and s["tag"] == sha and s["release"]["name"] == f"Latest build (0.1.0-{run})" \
    and f"run={run} sha={sha}" in s["release"]["body"]
if not ok:
    print("     have", sorted(have), s["tag"][:6], s["release"]["name"], file=sys.stderr)
sys.exit(0 if ok else 1)
EOF
}

# 1. First publish: no tag, no release.
build "$t/new" run42 a.rpm b.deb SHA256SUMS
echo '{"tag": null, "compare": {}, "fail": {}, "counts": {}, "next_id": 500, "release": null}' >"$MOCK_STATE"
run $NEW 42; rc=$?
check "first publish creates tag and release" test $rc -eq 0
check "first publish has the build" is_new $NEW 42

# 2. Newer build replaces files and drops the stale one.
published
run $NEW 42; rc=$?
check "newer build publishes" test $rc -eq 0
check "newer build replaces everything, drops stale, leaves no staging" is_new $NEW 42

# 3. Re-run of an older commit is skipped.
published
set_state "s['compare']['$OLD...$NEW']='behind'"
cp "$MOCK_STATE" "$t/before.json"
run $NEW 40; rc=$?
check "older commit exits 0" test $rc -eq 0
check "older commit changes nothing" unchanged

# 4. Same commit: lower run skipped, same run republished.
published
set_state "s['compare']['$OLD...$OLD']='identical'"
cp "$MOCK_STATE" "$t/before.json"
run $OLD 40; rc=$?
check "same commit, older run is skipped" eval "[ $rc -eq 0 ] && unchanged"
run $OLD 41; rc=$?
check "same commit, same run republishes" eval "[ $rc -eq 0 ] && is_new $OLD 41"

# 5. Upload fails on the second file: nothing changes.
published
set_state "s['fail']['upload']=2"
run $NEW 42; rc=$?
check "upload failure exits 1" test $rc -eq 1
check "upload failure leaves release as it was" unchanged

# 6. Rename fails part way through the swap: everything restored.
for n in 1 2 4 5 6; do
    published
    set_state "s['fail']['rename']=$n"
    run $NEW 42; rc=$?
    check "rename failure $n exits 1" test $rc -eq 1
    check "rename failure $n restores the old build" unchanged
done

# 7. Tag move fails after the swap: everything restored.
published
set_state "s['fail']['tagpatch']=True"
run $NEW 42; rc=$?
check "tag move failure exits 1" test $rc -eq 1
check "tag move failure restores files, tag and notes" unchanged

# 8. main was reset and republished: a lower run on a commit that's ahead is
#    still skipped.
published
set_state "s['compare']['$OLD...$NEW']='ahead'"
cp "$MOCK_STATE" "$t/before.json"
run $NEW 40; rc=$?
check "lower run on a newer commit is skipped" eval "[ $rc -eq 0 ] && unchanged"

# 9. An undo step fails too: nothing serving a download is deleted, every old
#    file survives, each name keeps a live file or both copies, and it exits 2.
#    Renames run aside/promote per file, so 4 fails the second promote and
#    5 or 6 fail the undo of the first promote or the second aside.
nothing_lost() {
    python3 - "$t" <<'EOF'
import json, os, sys
t = sys.argv[1]
b = json.load(open(f"{t}/before.json"))["release"]["assets"]
s = json.load(open(f"{t}/state.json"))["release"]["assets"]
have = {a["sha"] for a in s}
names = {a["name"] for a in s}
lost = [a["name"] for a in b if a["sha"] not in have]
def kept(prefix, n):
    return any(x.startswith(prefix) and x.endswith("." + n) for x in names)
# A name without a live file must keep both the previous and the staged copy.
gone = [n for n in os.listdir(f"{t}/new")
        if n not in names and not (kept("previous-", n) and kept("staging-", n))]
if lost or gone:
    print("     lost", lost, "gone", gone, sorted(names), file=sys.stderr)
sys.exit(1 if lost or gone else 0)
EOF
}
for n in 5 6; do
    published
    set_state "s['fail']['rename']=[4, $n]"
    run $NEW 42; rc=$?
    check "failed undo $n exits 2 and says so" eval "[ $rc -eq 2 ] && grep -q incomplete '$t/out.log'"
    check "failed undo $n deletes nothing still needed" nothing_lost
done

rm -rf "$t"
echo "$fails failed"
exit $fails
