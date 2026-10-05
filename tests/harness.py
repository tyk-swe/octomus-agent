"""The e2e harness: the real service against deterministic external peers.

tests/e2e.py, tests/e2e_sandbox.py and tests/distribution.py import it; it runs no scenario
itself. No network writes, real model turns, credentials or spending. Run the suites after
`make build` (dashboard + Go binary) or set OCTOMUS_TEST_BINARY.
"""
import contextlib
from concurrent.futures import ThreadPoolExecutor, as_completed
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import tempfile
import time
import traceback
import urllib.error
import urllib.request

PROJECT = Path(__file__).resolve().parents[1]
BINARY = Path(os.environ.get('OCTOMUS_TEST_BINARY', str(PROJECT / 'bin/octomus-agent')))
TOKEN = 'fixture-operator-token-with-at-least-32-characters'
CODEX_ROUTE = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
FEATURE_CHECK = 'for file in feature*.txt; do test "$(cat "$file")" = fixed || exit 1; done'
HOLDS = ['audit-hold']
LOCAL_HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def local_urlopen(request, *, timeout):
    """Open a local fixture request directly, regardless of ambient proxy settings."""
    return LOCAL_HTTP.open(request, timeout=timeout)


def fixture_git_environment():
    """A child-only Git environment for the fixture's local repositories.

    Ambient configuration may require signing or run user hooks, and GIT_DIR,
    GIT_WORK_TREE and GIT_INDEX_FILE can redirect writes outside the fixture.
    Repository-local configuration still applies; fixture_service's explicit
    environment overrides are applied after this default.
    """
    env = {key: value for key, value in os.environ.items() if not key.startswith('GIT_')}
    env.update(GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull)
    return env


def git(*args, cwd):
    return subprocess.check_output(['/usr/bin/git', *args], cwd=cwd, env=fixture_git_environment(), stderr=subprocess.DEVNULL, text=True).strip()


def poll(predicate, seconds, interval=0.1, tick=None):
    """Runs `predicate` until it returns a truthy value or `seconds` elapse.

    Returns that value, or None on timeout. A predicate that cannot connect is retried,
    because the service may still be starting; `tick` runs once per iteration and may
    raise to abort the wait early.
    """
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            result = predicate()
            if result:
                return result
        except (OSError, urllib.error.URLError):
            pass
        if tick:
            tick()
        time.sleep(interval)
    return None


def service_log(root, tail=None):
    """Returns root/service.log for failure messages, only its last `tail` lines when given."""
    try:
        text = (root / 'service.log').read_text(errors='replace')
    except OSError as error:
        return f'<service.log unavailable: {error!r}>'
    return text if tail is None else '\n'.join(text.splitlines()[-tail:])


def run_selected(suite, scenarios, names, *, workers=None, output=None):
    """Runs `names`, or every scenario when it is empty, with bounded parallelism.

    `scenarios` lists (name, zero-argument callable) pairs; unknown names are
    refused before anything runs. Each scenario owns its fixtures. Set
    OCTOMUS_TEST_JOBS=1 for registry-order serial execution; the default is up
    to four workers. All running scenarios finish teardown before failure returns.
    """
    registry = dict(scenarios)
    assert len(registry) == len(scenarios), f'duplicate {suite} scenario names'
    unknown = [name for name in names if name not in registry]
    if unknown:
        raise SystemExit(f'unknown {suite} scenarios: {", ".join(unknown)}; available: {", ".join(registry)}')
    if workers is None:
        try:
            workers = int(os.environ.get('OCTOMUS_TEST_JOBS', min(4, os.cpu_count() or 1)))
        except ValueError:
            raise SystemExit('OCTOMUS_TEST_JOBS must be a positive integer') from None
    if workers < 1:
        raise SystemExit('OCTOMUS_TEST_JOBS must be a positive integer')
    selected = [(name, run) for name, run in registry.items() if not names or name in names]

    def execute(name, run):
        print(f'RUN {suite} {name}', flush=True, file=output)
        run()
        print(f'PASS {suite} {name}', flush=True, file=output)

    if workers == 1 or len(selected) <= 1:
        for name, run in selected:
            execute(name, run)
        return

    failed = []
    with ThreadPoolExecutor(max_workers=min(workers, len(selected))) as pool:
        pending = {pool.submit(execute, name, run): name for name, run in selected}
        for future in as_completed(pending):
            try:
                future.result()
            except Exception as error:
                name = pending[future]
                failed.append(name)
                print(f'FAIL {suite} {name}', flush=True, file=output)
                traceback.print_exception(type(error), error, error.__traceback__, file=output)
    if failed:
        raise SystemExit(f'{suite} failed scenarios: {", ".join(sorted(failed))}')


def routes(executor='codex', planning=None, reviewer=None, repair=None):
    """The route fields of a configuration: `executor` backs the tiers and, unless given, every other role.

    OpenCode planning roles use the variant-free `plain-model`; an OpenCode repair route uses
    the `alternate` provider without a variant. An all-OpenCode selection also points the
    Codex binary at a path that does not exist, so nothing can fall back to it.
    """
    planning, reviewer, repair = planning or executor, reviewer or executor, repair or executor

    def route(backend, planning=False, provider='fixture', variant='high'):
        if backend == 'codex':
            return dict(CODEX_ROUTE)
        return {'backend': 'opencode', 'provider': provider, 'model': 'plain-model' if planning else 'fixture-model', 'effort': '', **({'variant': variant} if variant and not planning else {})}

    selected = {'roles': {role: route(planning, planning=True) for role in ['orchestrator', 'discovery', 'proposal_reviewer']},
                'tiers': {tier: route(executor) for tier in ['XS', 'S', 'M', 'L', 'XL']},
                'repair_route': route(repair, provider='alternate', variant=None)}
    selected['roles']['code_reviewer'] = route(reviewer)
    if {planning, executor, reviewer, repair} == {'opencode'}:
        selected['codex_binary'] = '/codex-is-not-installed'
    return selected


class Service:
    def __init__(self, root):
        self.root = root
        self.process = None
        self.stopped_process = None
        self.log = (root / 'service.log').open('a')
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            self.port = sock.getsockname()[1]
        self.env = fixture_git_environment()
        self.env.pop('OCTOMUS_NOTIFICATION_WEBHOOK_URL', None)
        # Fixture runners are host scripts; tests/e2e_sandbox.py covers the Docker sandbox.
        self.env.update({'OCTOMUS_TOKEN': TOKEN, 'OCTOMUS_FIXTURE': str(root), 'OCTOMUS_SANDBOX': 'off', 'PATH': f'{root / "bin"}:{os.environ["PATH"]}'})

    def start(self):
        self.process = subprocess.Popen([str(BINARY), '--data-dir', str(self.root / '.octomus'), '--listen', f'127.0.0.1:{self.port}', '--assets', str(PROJECT / 'web/build')], env=self.env, stdout=self.log, stderr=self.log)
        self.wait(lambda: self.request('/healthz', api=False), 'service startup')

    def stop(self, crash=False):
        process = self.process
        if process is None or process is self.stopped_process:
            return
        requested_kill = False
        if process.poll() is None:
            if crash:
                process.kill()
                requested_kill = True
            else:
                process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
                self.stopped_process = process
                raise AssertionError(f'service did not stop within 15s of {"SIGKILL" if crash else "SIGTERM"}; service.log tail:\n{service_log(self.root, tail=100)}')
        # Teardown can run again after a scenario already stopped this process.
        # Report a failure once, while checking every replacement after restart.
        self.stopped_process = process
        if process.returncode != 0 and not (requested_kill and process.returncode == -signal.SIGKILL):
            raise AssertionError(f'service exited with status {process.returncode}; service.log tail:\n{service_log(self.root, tail=100)}')

    def _open(self, path, method, value, api, timeout):
        """Sends one authenticated request; `timeout` bounds each socket operation,
        so a response the service holds longer than that raises TimeoutError."""
        request = urllib.request.Request(f'http://127.0.0.1:{self.port}{"/api" if api else ""}{path}', method=method, headers={'Authorization': f'Bearer {TOKEN}', 'Content-Type': 'application/json'}, data=json.dumps(value or {}).encode() if method != 'GET' else None)
        return local_urlopen(request, timeout=timeout)

    def request(self, path, method='GET', value=None, api=True, timeout=5):
        """One request that must succeed: returns the JSON body, raises HTTPError otherwise."""
        with self._open(path, method, value, api, timeout) as response:
            return json.load(response)

    def expect(self, path, method='GET', value=None, timeout=5):
        """One API request that may fail: returns (status, body) instead of raising."""
        try:
            with self._open(path, method, value, True, timeout) as response:
                return response.status, json.load(response)
        except urllib.error.HTTPError as error:
            return error.code, json.loads(error.read() or b'{}')

    def save_config(self, config):
        """Replaces the given top-level fields under the current canonical
        revision and returns the fresh settings view.

        `config` is sent as the `config` patch: callers that mutate a loaded
        display view send every field back, while precise callers may pass a
        partial map. Each scenario owns its service, so the revision read here
        is the one the caller loaded.
        """
        revision = self.request('/config')['revision']
        return self.request('/config', 'PUT', {'expected_revision': revision, 'config': config})

    def configure(self, routes, commands=(FEATURE_CHECK,), start=True, **overrides):
        """Saves the scenario configuration (`routes` from routes(), the verification
        `commands`, fixture repository and timeouts) and returns the saved view.
        Unless `start` is false, it then requests one cycle."""
        config = self.request('/config')['config']
        config.update({'repository': str(self.root / 'checkout'), 'github_repo': 'fixture/project', 'verification_commands': list(commands), 'session_timeout_seconds': 30, 'command_timeout_seconds': 10, 'cycle_interval_seconds': 3600, 'task_timeout_seconds': 120, **routes, **overrides})
        view = self.save_config(config)
        if start:
            self.request('/control/cycle', 'POST')
        return view

    def wait(self, predicate, label, seconds=45):
        last_error = None

        def attempt():
            nonlocal last_error
            try:
                return predicate()
            except urllib.error.HTTPError as error:
                try:
                    body = error.read()[:2000].decode(errors='replace')
                except Exception as read_error:
                    body = f'<body unavailable: {read_error!r}>'
                last_error = f'HTTP {error.code}: {body}'
                raise
            except (OSError, urllib.error.URLError) as error:
                last_error = repr(error)
                raise

        def exited():
            if self.process and self.process.poll() is not None:
                raise AssertionError(f'{label}: service exited\n{service_log(self.root)}')
        result = poll(attempt, seconds, tick=exited)
        if result:
            return result
        try:
            state = json.dumps(self.request('/state'), indent=2)
        except Exception as error:
            state = f'<state unavailable: {error!r}>'
        raise AssertionError(f'{label} timed out after {seconds}s; last error: {last_error}\nstate: {state}\nservice.log tail:\n{service_log(self.root, tail=100)}')

    def terminal_task(self):
        state = self.request('/state')
        assert not state['control']['error'], state['control']['error']
        tasks = state['tasks']
        return self.request(f'/tasks/{tasks[0]["id"]}') if tasks and tasks[0]['status'] in ['published', 'blocked', 'failed'] else None


def setup(root):
    (root / 'bin').mkdir()
    for name, fixture in [('codex', 'codex.py'), ('opencode', 'opencode.py'), ('gh', 'gh.py'), ('git', 'git.sh')]:
        dest = root / 'bin' / name
        shutil.copy(PROJECT / 'tests/fixtures' / fixture, dest)
        dest.chmod(0o755)
    shutil.copy(PROJECT / 'tests/fixtures/worker.py', root / 'bin/worker.py')
    (root / 'checkout').mkdir()
    git('init', '--bare', str(root / 'remote.git'), cwd=root)
    git('init', '-b', 'main', cwd=root / 'checkout')
    git('config', 'user.name', 'Fixture', cwd=root / 'checkout')
    git('config', 'user.email', 'fixture@example.com', cwd=root / 'checkout')
    (root / 'checkout/README.md').write_text('Feature contract: feature.txt must contain fixed.\n')
    git('add', '.', cwd=root / 'checkout')
    git('commit', '-m', 'Initial fixture', cwd=root / 'checkout')
    git('remote', 'add', 'origin', str(root / 'remote.git'), cwd=root / 'checkout')
    git('push', '-u', 'origin', 'main', cwd=root / 'checkout')
    git('symbolic-ref', 'HEAD', 'refs/heads/main', cwd=root / 'remote.git')


def stop_peers(root):
    """Kills the OpenCode peer process groups a scenario left behind."""
    path = root / 'opencode-pids.jsonl'
    pids = [json.loads(row)['pid'] for row in path.read_text().splitlines()] if path.exists() else []
    for pid in pids:
        try:
            args = Path(f'/proc/{pid}/cmdline').read_bytes()
            if str(root).encode() in args and os.getpgid(pid) == pid:
                os.killpg(pid, signal.SIGKILL)
        except (FileNotFoundError, ProcessLookupError):
            pass


def release_holds(root):
    """Removes every hold marker, so no held fixture peer outlasts its scenario."""
    for name in HOLDS:
        (root / name).unlink(missing_ok=True)


@contextlib.contextmanager
def fixture_service(prefix, prepare=None, env=None, start=True):
    """Yields (root, service) for one scenario in a fresh fixture directory.

    `prepare(root)` runs after setup, before the service exists: it writes the
    markers and saved PRs the service must find at startup. `env` adds service
    environment variables. With `start` false the scenario starts the service
    itself. Teardown runs even after a failure, in this order: holds are
    released, the service is stopped, OpenCode peers are killed and the log is
    closed; then the directory is removed.
    """
    with tempfile.TemporaryDirectory(prefix=prefix) as tmp, contextlib.ExitStack() as teardown:
        root = Path(tmp)
        setup(root)
        if prepare:
            prepare(root)
        service = Service(root)
        teardown.callback(service.log.close)
        teardown.callback(stop_peers, root)
        teardown.callback(service.stop)
        teardown.callback(release_holds, root)
        if env:
            service.env.update(env)
        if start:
            service.start()
        yield root, service
