#!/usr/bin/env python3
"""验收给定发布压缩包；不构建 km，不修改仓库，只操作临时项目与本次派生镜像。"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import pty
import re
import select
import shutil
import signal
import subprocess
import tarfile
import tempfile
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--archive', type=Path, required=True)
    parser.add_argument('--checksums', type=Path, required=True)
    parser.add_argument('--expected-commit', required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--base-image', default='kali-mac-min:0.2')
    args = parser.parse_args()
    args.archive = args.archive.resolve()
    args.checksums = args.checksums.resolve()
    args.evidence = args.evidence.resolve()
    args.evidence.mkdir(parents=True, exist_ok=True)
    if platform.system() != 'Darwin' or platform.machine() != 'arm64':
        parser.error('本脚本对应 darwin/arm64 本机验收；其他平台需独立实机记录')
    checks = []
    log = (args.evidence / 'steps.log').open('w', encoding='utf-8')
    work = Path(tempfile.mkdtemp(prefix='km-release-', dir=args.evidence))
    project = work / '中文 空格 项目'
    project.mkdir()
    project_id = None
    derived = 'km-release-drill:' + work.name
    image_built = False
    live = None
    passed = False

    def note(msg):
        print(msg, flush=True)
        log.write(msg + '\n')
        log.flush()

    def check(name, condition):
        if not condition:
            raise AssertionError(name)
        checks.append(name)
        note('PASS ' + name)

    def run(argv, cwd=project, expected=0, timeout=90, env=None):
        note('$ ' + repr([str(a) for a in argv]))
        result = subprocess.run([str(a) for a in argv], cwd=cwd, env=env,
                                text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                timeout=timeout)
        note(result.stdout + result.stderr + '\nEXIT=' + str(result.returncode))
        if expected is not None and result.returncode != expected:
            raise AssertionError(f'exit {result.returncode}, expected {expected}: {argv}')
        return result

    def km(*argv, expected=0):
        return run([binary, *argv], expected=expected)

    def active_id():
        result = km('sessions').stdout
        match = re.search(r'ACTIVE\s+(s[0-9a-f]{16})', result)
        return match.group(1) if match else None

    def pty_walkthrough():
        pid, fd = pty.fork()
        if pid == 0:
            os.chdir(project)
            os.environ['TERM'] = 'xterm-256color'
            os.execv(str(binary), [str(binary), 'shell'])
        reaped = False

        def receive(token, timeout=25):
            buf = b''
            deadline = time.monotonic() + timeout
            while token not in buf and time.monotonic() < deadline:
                ready, _, _ = select.select([fd], [], [], 0.2)
                if ready:
                    try:
                        data = os.read(fd, 65536)
                    except OSError:
                        break
                    if not data:
                        break
                    buf += data
            note('PTY ' + repr(buf.decode('utf-8', 'replace')))
            if token not in buf:
                raise AssertionError('PTY expected ' + repr(token))
            return buf

        try:
            receive(b'KM_SHELL> ')
            os.write(fd, b'pwd\n')
            check('PTY pwd=/workspace', b'/workspace\r\n' in receive(b'KM_SHELL> '))
            os.write(fd, b'python3 hello.py\n')
            check('PTY runs Mac file', b'hello from a Mac file\r\n' in receive(b'KM_SHELL> '))
            os.write(fd, b'sleep 120\n')
            receive(b'sleep 120')
            time.sleep(0.5)
            os.write(fd, b'\x03')
            receive(b'KM_SHELL> ')
            os.write(fd, b'echo RC=$?\n')
            check('PTY Ctrl-C=130', b'RC=130\r\n' in receive(b'KM_SHELL> '))
            os.write(fd, b'exit 7\n')
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                got, status = os.waitpid(pid, os.WNOHANG)
                if got:
                    reaped = True
                    check('PTY exit 7 passthrough', os.waitstatus_to_exitcode(status) == 7)
                    break
                time.sleep(0.1)
            if not reaped:
                raise AssertionError('PTY exit timeout')
        finally:
            if not reaped:
                os.kill(pid, signal.SIGKILL)
                os.waitpid(pid, 0)
            os.close(fd)

    try:
        digest = hashlib.sha256(args.archive.read_bytes()).hexdigest()
        sums = {line.split(maxsplit=1)[1].lstrip('*').removeprefix('./'): line.split()[0]
                for line in args.checksums.read_text().splitlines() if line.strip()}
        check('archive SHA256', sums[args.archive.name] == digest)
        with tarfile.open(args.archive, 'r:gz') as archive:
            members = archive.getmembers()
            check('archive contains only relative regular files/directories', all(
                not Path(m.name).is_absolute() and '..' not in Path(m.name).parts
                and (m.isfile() or m.isdir()) for m in members))
            archive.extractall(work / 'unpacked', members=members)
        packages = list((work / 'unpacked').iterdir())
        check('single package root', len(packages) == 1)
        package = packages[0]
        metadata = (package / 'version-metadata.txt').read_text()
        check('source SHA in metadata', 'code_rev: ' + args.expected_commit + '\n' in metadata)
        check('clean build metadata', 'worktree: clean\n' in metadata)
        version = re.search(r'^version: (.+)$', metadata, re.M).group(1)
        check('release version', version == '0.4.0-rc.1')
        check('quickstart and Dockerfile', (package / 'QUICKSTART.md').is_file()
              and (package / 'images/kali/Dockerfile').is_file())
        prefix = work / 'prefix'
        env = dict(os.environ, PREFIX=str(prefix))
        run(['bash', package / 'install.sh'], cwd=package, env=env)
        binary = prefix / 'bin/km'
        check('installed binary identical to archive', binary.read_bytes() == (package / 'km').read_bytes())
        identity = km('version', '--verbose').stdout
        check('installed build identity', f'commit: {args.expected_commit}\n' in identity
              and 'worktree: clean\n' in identity and 'target: darwin/arm64\n' in identity)
        run(['docker', 'info', '--format', '{{.ServerVersion}}'])
        base_id = run(['docker', 'image', 'inspect', args.base_image, '--format', '{{.Id}}']).stdout.strip()
        km('init', '--image', args.base_image)
        state = json.loads((project / '.km/state.json').read_text())
        project_id = state['project_id']
        original_id = state['container']['id']
        check('project identity recorded', bool(re.fullmatch(r'p[0-9a-f]+', project_id)))
        check('hello command', km('run', '--', 'python3', '-c', 'print("hello from Kali")').stdout.strip() == 'hello from Kali')
        check('root mapping', km('run', '--', 'pwd').stdout.strip() == '/workspace')
        (project / 'hello.py').write_text('print("hello from a Mac file")\n')
        check('Mac file runs inside container', km('python3', 'hello.py').stdout.strip() == 'hello from a Mac file')
        km('python3', '-c', 'from pathlib import Path; Path("result.txt").write_text("made in Kali\\n")')
        check('container file visible on Mac', (project / 'result.txt').read_text() == 'made in Kali\n')
        check('idle status', json.loads(km('status', '--json').stdout)['state'] == 'running_idle')
        tools = km('tools').stdout
        check('six tools AVAILABLE', len(re.findall(r'^\s+AVAILABLE\s', tools, re.M)) == 6)
        check('CURRENT listed', 'CURRENT' in km('env', 'list').stdout)
        pty_walkthrough()
        km('stop')
        check('stopped status', json.loads(km('status', '--json').stdout)['state'] == 'container_stopped')
        check('automatic restart', km('python3', 'hello.py').stdout.strip() == 'hello from a Mac file')
        check('project result survives stop/start', (project / 'result.txt').read_text() == 'made in Kali\n')

        task_log = (args.evidence / 'long-task.log').open('w')
        try:
            live = subprocess.Popen([str(binary), 'run', '--', 'sleep', '120'], cwd=project,
                                    stdout=task_log, stderr=task_log, start_new_session=True)
            deadline = time.monotonic() + 20
            sid = None
            while not sid and time.monotonic() < deadline:
                sid = active_id()
                if not sid:
                    time.sleep(0.2)
            check('long task registered ACTIVE', sid is not None)
            os.killpg(live.pid, signal.SIGKILL)
            check('host client really SIGKILLed', live.wait(timeout=10) == -signal.SIGKILL)
            live = None
            blocked = km('run', '--', 'true', expected=1)
            check('surviving task protects execution', 'KM_SESSION_ACTIVE' in blocked.stderr)
            check('same session remains visible', active_id() == sid)
            km('cancel', sid)
            check('explicit cancel clears ACTIVE', active_id() is None)
            check('execution recovered', km('run', '--', 'printf', 'recovered\n').stdout == 'recovered\n')
        finally:
            task_log.close()

        build_dir = work / 'image'
        build_dir.mkdir()
        (build_dir / 'Dockerfile').write_text(f'FROM {base_id}\nRUN mkdir -p /opt/km-release && printf "B\\n" > /opt/km-release/marker\n')
        run(['docker', 'build', '-q', '-t', derived, build_dir], timeout=120)
        image_built = True
        before = {p.relative_to(project).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
                  for p in project.rglob('*') if p.is_file()}
        km('env', 'switch', '--image', derived, '--dry-run')
        after = {p.relative_to(project).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
                 for p in project.rglob('*') if p.is_file()}
        check('dry-run leaves project files unchanged', before == after)
        km('env', 'switch', '--image', derived, '--yes')
        check('new environment executed', km('run', '--', 'cat', '/opt/km-release/marker').stdout.strip() == 'B')
        new_id = json.loads((project / '.km/state.json').read_text())['container']['id']
        rows = km('env', 'list').stdout
        check('current and previous roles', 'CURRENT' in rows and 'PREVIOUS' in rows)
        check('current protected', 'KM_' in (km('env', 'remove', new_id, '--yes', expected=1).stderr))
        check('previous protected', 'KM_' in (km('env', 'remove', original_id, '--yes', expected=1).stderr))
        km('python3', '-c', 'from pathlib import Path; Path("during-B.txt").write_text("keep after rollback\\n")')
        km('env', 'rollback', '--yes')
        check('rollback restores original container', json.loads((project / '.km/state.json').read_text())['container']['id'] == original_id)
        check('rollback preserves shared file changes', (project / 'during-B.txt').read_text() == 'keep after rollback\n')
        check('withdrawn environment retained', 'RETAINED' in km('env', 'list').stdout)
        km('env', 'remove', new_id, '--dry-run')
        km('env', 'remove', new_id, '--yes')
        ids = run(['docker', 'ps', '-aq', '--no-trunc', '--filter', 'label=km.project=' + project_id]).stdout.split()
        check('removed target absent; only current remains', ids == [original_id])
        km('env', 'remove', new_id, '--yes', expected=1)
        check('derived image preserved by env remove', run(['docker', 'image', 'inspect', derived, '--format', '{{.Id}}']).stdout.strip().startswith('sha256:'))
        check('recover without transaction is informational', '无需恢复' in km('env', 'recover').stdout)
        check('current still executes after cleanup', km('python3', 'hello.py').stdout.strip() == 'hello from a Mac file')
        diagnostic = km('doctor').stdout
        check('doctor has zero failures/warnings', bool(re.search(r'结论: \d+ 通过, 0 警告, 0 失败', diagnostic)))
        km('stop')
        manifest = (prefix / 'share/km/manifest.txt').read_text().splitlines()
        canary = prefix / 'keep-user-file.txt'
        canary.write_text('KEEP\n')
        run(['bash', package / 'uninstall.sh'], cwd=package, env=env)
        check('uninstall removes manifest members', all(not (prefix / f).exists() for f in manifest))
        check('uninstall preserves canary and project', canary.read_text() == 'KEEP\n' and (project / 'hello.py').exists())
        passed = True
    finally:
        if live and live.poll() is None:
            os.killpg(live.pid, signal.SIGKILL)
            live.wait(timeout=10)
        try:
            if project_id is None and (project / '.km/state.json').exists():
                project_id = json.loads((project / '.km/state.json').read_text())['project_id']
            if project_id:
                ids = run(['docker', 'ps', '-aq', '--no-trunc', '--filter', 'label=km.project=' + project_id]).stdout.split()
                if ids:
                    run(['docker', 'rm', '-f', *ids])
                left = run(['docker', 'ps', '-aq', '--no-trunc', '--filter', 'label=km.project=' + project_id]).stdout.strip()
                check('test containers: zero residue', left == '')
            if image_built:
                run(['docker', 'image', 'rm', derived])
            (args.evidence / 'summary.json').write_text(json.dumps({
                'passed': passed, 'checks': checks, 'count': len(checks),
                'archive': str(args.archive), 'sha256': hashlib.sha256(args.archive.read_bytes()).hexdigest(),
                'expected_commit': args.expected_commit, 'platform': 'darwin/arm64',
                'base_image': args.base_image, 'base_image_rebuilt': False,
                'amd64_hardware_validated': False,
            }, ensure_ascii=False, indent=2) + '\n')
        finally:
            log.close()
            shutil.rmtree(work)
    if passed:
        print(f'RELEASE_WALKTHROUGH_PASS checks={len(checks)}')


if __name__ == '__main__':
    main()
