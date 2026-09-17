#!/bin/bash
# 二轮收口安装版验收（自包含：不依赖调用方 shell 状态）
set -u
MAIN="/Users/y4n9/Workspace/Projects/My-github-projects/KaliMac"
EV="$MAIN/tests/evidence/session-recovery"
STAGE=$(mktemp -d /tmp/km-r2.XXXXXX)
DESTDIR="$STAGE/prefix" PREFIX=/opt/km-r2 KM_BIN="$MAIN/dist/km-0.4.0-p3-darwin-arm64" "$MAIN/scripts/install.sh" | head -1
KMB="$STAGE/prefix/opt/km-r2/bin/km"
"$KMB" --version || exit 1
DEMO=$(mktemp -d "$HOME/Workspace/km r2.XXXXXX")
cd "$DEMO" || exit 1
"$KMB" init --image kali-mac-min:0.2 | head -1
"$KMB" status | sed -n '2p'
"$KMB" run -- /bin/sh -c 'sleep 60' >/dev/null 2>&1 &
BGPID=$!
sleep 3
pkill -9 -P $BGPID 2>/dev/null
kill -9 $BGPID 2>/dev/null
sleep 1
"$KMB" run -- /bin/echo X >/dev/null 2>&1
echo "blocked-rc=$?"
CID=$(python3 -c "import json;print(json.load(open('.km/state.json'))['container']['id'])")
SID=$(docker exec "$CID" /tmp/km-bin/km-ctl sessions | awk '/^ACTIVE /{print $2; exit}')
"$KMB" cancel "$SID"
echo "cancel-rc=$?"
"$KMB" run -- /bin/echo R2_RECOVERED
echo "recover-rc=$?"
( cd "$MAIN" && DESTDIR="$STAGE/prefix" PREFIX=/opt/km-r2 scripts/uninstall.sh | tail -1 )
# 清理（先读 ID 后删目录）
CID2=$(python3 -c "import json;print(json.load(open('$DEMO/.km/state.json'))['container']['id'])" 2>/dev/null)
[ -n "$CID2" ] && docker rm -f "$CID2" >/dev/null 2>&1 && echo "removed $CID2"
rm -rf "$DEMO" "$STAGE"
