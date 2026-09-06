package session

import "strings"

// Container-side paths for the session kernel.
const (
	// ScriptsDir is where the bootstrap installs controller scripts. /tmp is
	// present in every image; scripts are re-installed idempotently per run.
	ScriptsDir = "/tmp/km-bin"
	// SessionsDirBase holds per-execution session state inside the container.
	SessionsDirBase = "/tmp/km-sessions"
	// RunScriptPath / CtlScriptPath are the entry points km invokes.
	RunScriptPath = ScriptsDir + "/km-run"
	CtlScriptPath = ScriptsDir + "/km-ctl"
)

// kmRunSh is the per-execution supervisor. It puts the tool into its own
// session (setsid), records the pid, reaps the tool and records the exit
// code. It never traps signals: cancellation is performed by km-ctl killing
// the tool's process group, after which wait returns and the supervisor
// finalizes. stdin/stdout/stderr are inherited by the tool (streamed).
// Usage: km-run <sid> <tool> <args...>   Exit codes: 90/91 supervisor setup.
// POSIX 把后台任务的 stdin 指到 /dev/null，故先把原始 stdin 存到 fd3，
// 再让工具显式从 fd3 读取（实验验证过的接力方式）。
const kmRunSh = `#!/bin/sh
SID="$1"; shift
DIR="__SESSIONS__/$SID"
mkdir -p "$DIR" || exit 90
exec 3<&0
setsid "$@" <&3 &
TPID=$!
printf '%s\n' "$TPID" > "$DIR/pid" || exit 91
wait "$TPID"
CODE=$?
# 会话终态与进程组实际结束绑定：主进程退出≠组清空（同组子进程可能忽略
# TERM）。排空（TERM→KILL 有界升级）完成后再写终态、删目录。
# 逃逸出本进程组的守护化子进程不在保证范围（见 ADR-004）。
group_busy() {
  for p in /proc/[0-9]*; do
    read -r pid comm st ppid pg rest < "$p/stat" 2>/dev/null || continue
    [ "$st" = "Z" ] && continue
    [ "$pg" = "$TPID" ] && return 0
  done
  return 1
}
kill -TERM "-$TPID" 2>/dev/null || true
i=0; BUSY=1
while [ $i -lt 20 ]; do
  group_busy || { BUSY=0; break; }
  sleep 0.1; i=$((i+1))
done
if [ "$BUSY" = 1 ]; then
  kill -KILL "-$TPID" 2>/dev/null || true
  i=0
  while [ $i -lt 20 ]; do
    group_busy || { BUSY=0; break; }
    sleep 0.1; i=$((i+1))
  done
fi
printf '%s\n' "$CODE" > "$DIR/exit" 2>/dev/null || { mkdir -p "$DIR"; printf '%s\n' "$CODE" > "$DIR/exit"; }
rm -rf "$DIR"
exit "$CODE"
`

// kmCtlSh cancels one session. It targets only the recorded pid's process
// group, waits for the supervisor to reap (exit file), escalates to KILL,
// and reports via its exit status:
//
//	0 = session finalized (canceled, or had already finished)
//	3 = session dir/pid never appeared or session fully gone (idempotent)
//	4 = cleanup not confirmed within the time budget
//
// Usage: km-ctl cancel <sid>  （也兼容 km-ctl <sid>）
const kmCtlSh = `#!/bin/sh
# 会话成员判定（review C）：以 SID 域为准——同一 PTY 会话内的全部进程
# （bash、所有作业组成员、管道成员）SID 相同；setsid 主动脱离者不在域内，
# 属于记录在案的逃逸者。仅用 bash 的 PGID 会漏掉其他作业组。
sess_busy() {
  for p in /proc/[0-9]*; do
    read -r pid comm st ppid pgrp sess rest < "$p/stat" 2>/dev/null || continue
    [ "$st" = "Z" ] && continue
    [ "$sess" = "$TPID" ] && return 0
  done
  return 1
}
# 按 SID 域清理：TERM 全部成员 → 有界轮询 → KILL 兜底。
sess_kill() {
  for p in /proc/[0-9]*; do
    read -r pid comm st ppid pgrp sess rest < "$p/stat" 2>/dev/null || continue
    [ "$st" = "Z" ] && continue
    [ "$sess" = "$TPID" ] && kill -TERM "$pid" 2>/dev/null
  done
  i=0
  while sess_busy && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
  sess_busy || return 0
  for p in /proc/[0-9]*; do
    read -r pid comm st ppid pgrp sess rest < "$p/stat" 2>/dev/null || continue
    [ "$st" = "Z" ] && continue
    [ "$sess" = "$TPID" ] && kill -KILL "$pid" 2>/dev/null
  done
  i=0
  while sess_busy && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
  sess_busy && return 1 || return 0
}
CMD="$1"
shift
BASE="__SESSIONS__"
case "$CMD" in
cancel)
  SID="$1"
  DIR="$BASE/$SID"
  i=0
  while [ ! -d "$DIR" ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i+1)); done
  [ -d "$DIR" ] || exit 3
  i=0
  while [ ! -f "$DIR/pid" ] && [ $i -lt 50 ]; do
    [ ! -d "$DIR" ] && exit 0
    sleep 0.1; i=$((i+1))
  done
  [ -f "$DIR/pid" ] || { rm -rf "$DIR"; exit 3; }
  TPID=$(cat "$DIR/pid")
  [ -f "$DIR/exit" ] && { rm -rf "$DIR"; exit 0; }
  if sess_kill; then rm -rf "$DIR"; exit 0; fi
  exit 4
  ;;
alive)
  SID="$1"
  DIR="$BASE/$SID"
  [ -f "$DIR/pid" ] || exit 3
  TPID=$(cat "$DIR/pid")
  # 排除僵尸态：bash 退出后 /proc 短暂残留（Z），不算存活
  ST=$(cut -d" " -f3 "/proc/$TPID/stat" 2>/dev/null)
  if [ -d "/proc/$TPID" ] && [ "$ST" != "Z" ]; then exit 0; else exit 1; fi
  ;;
sessions)
  for d in "$BASE"/*/; do
    [ -d "$d" ] || continue
    sid=$(basename "$d")
    TPID=$(cat "$d/pid" 2>/dev/null) || { echo "STALE $sid"; continue; }
    if sess_busy; then echo "ACTIVE $sid"; else echo "STALE $sid"; fi
  done
  exit 0
  ;;
sweep)
  n=0
  for d in "$BASE"/*/; do
    [ -d "$d" ] || continue
    sid=$(basename "$d")
    TPID=$(cat "$d/pid" 2>/dev/null) || { rm -rf "$d"; continue; }
    if sess_busy; then n=$((n+1)); else rm -rf "$d"; fi
  done
  echo "$n"
  exit 0
  ;;
esac
exit 2
`

// kmShellSh 登记交互 shell 会话后以交互 bash 替换自身。
// 控制终端由 docker exec -t 提供；PS1/PROMPT_COMMAND 由调用方经 -e 注入。
// Usage: km-shell <sid>
const kmShellSh = `#!/bin/sh
SID="$1"
DIR="__SESSIONS__/$SID"
mkdir -p "$DIR" || exit 90
printf '%s\n' "$$" > "$DIR/pid" || exit 91
echo shell > "$DIR/kind"
exec bash --noprofile --norc -i
`

// kmObserveSh is a test/diagnostic helper: independent, /proc-based process
// observation scoped by process group — no name matching, no procps.
// Usage: km-observe all | km-observe pgid:<n>
const kmObserveSh = `#!/bin/sh
PATTERN="$1"
for p in /proc/[0-9]*; do
  read -r pid comm state ppid pgrp rest < "$p/stat" 2>/dev/null || continue
  case "$PATTERN" in
    all) printf '%s %s %s %s\n' "$pid" "$state" "$pgrp" "${comm#*(}" ;;
    pgid:*) [ "$pgrp" = "${PATTERN#pgid:}" ] && printf '%s %s %s %s\n' "$pid" "$state" "$pgrp" "${comm#*(}" ;;
  esac
done
`

// scripts returns the files installed by Bootstrap: path → content.
func scripts() map[string]string {
	replace := func(s string) string { return strings.ReplaceAll(s, "__SESSIONS__", SessionsDirBase) }
	return map[string]string{
		RunScriptPath:              replace(kmRunSh),
		CtlScriptPath:              replace(kmCtlSh),
		ScriptsDir + "/km-shell":   replace(kmShellSh),
		ScriptsDir + "/km-observe": replace(kmObserveSh),
	}
}
