"""The e2e harness: the real service against deterministic external peers.

Every e2e suite and the distribution test import it; it runs no scenario itself.
No network writes, real Codex turns, credentials, or spending. Run the suites after
make build (dashboard + Go binary) or set OCTOMUS_TEST_BINARY.
"""
import contextlib
from concurrent.futures import ThreadPoolExecutor, as_completed
import fcntl
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
RACE_EXIT_STATUS = 66
CODEX_ROUTE = {'backend': 'codex', 'model': 'gpt-6-astra', 'effort': 'medium'}
HOLDS = ['reconcile-hold', 'audit-hold']


def git(*args, cwd):
    return subprocess.check_output(['/usr/bin/git', *args], cwd=cwd, stderr=subprocess.DEVNULL, text=True).strip()


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


def select_scenarios(suites, names):
    """Returns qualified scenarios selected by suite or `suite/scenario` name.

    `suites` is an ordered list of (suite alias, scenarios) pairs whose
    scenarios are (name, zero-argument callable) pairs; the result keeps that
    order and qualifies every name as `suite/name`. With no `names` everything
    is selected; otherwise each name is either an exact suite alias, which
    expands the whole suite, or an exact qualified scenario. Every unknown name
    is refused before anything runs, and overlapping selections still run a
    scenario only once.
    """
    aliases = []
    qualified = []
    seen_suites = set()
    seen_qualified = set()
    for suite, scenarios in suites:
        if suite in seen_suites:
            raise SystemExit(f'duplicate suite name: {suite}')
        seen_suites.add(suite)
        aliases.append(suite)
        registry = dict(scenarios)
        if len(registry) != len(scenarios):
            raise SystemExit(f'duplicate {suite} scenario names')
        for name, run in scenarios:
            qualified_name = f'{suite}/{name}'
            if qualified_name in seen_qualified:
                raise SystemExit(f'duplicate scenario name: {qualified_name}')
            seen_qualified.add(qualified_name)
            qualified.append((qualified_name, run))
    available = [name for name, _ in qualified]
    unknown = [name for name in names if name not in seen_suites and name not in seen_qualified]
    if unknown:
        raise SystemExit(f'unknown scenarios: {", ".join(unknown)}; available suites: {", ".join(aliases)}; scenarios: {", ".join(available)}')
    selected = set(available) if not names else set()
    for name in names:
        if name in seen_suites:
            selected.update(qualified_name for qualified_name in available if qualified_name.startswith(f'{name}/'))
        else:
            selected.add(name)
    return [(name, run) for name, run in qualified if name in selected]


def process_gone(pid):
    """Whether `pid` has exited: reaped, or a zombie its parent has not reaped yet.

    The state is the first field after the last ')', because the command name
    before it is arbitrary text and may itself contain ') Z'.
    """
    try:
        stat = Path(f'/proc/{pid}/stat').read_text()
    except (FileNotFoundError, ProcessLookupError):
        return True
    return stat.rpartition(')')[2].split()[0] in ['Z', 'X']


def base_config(service, commands, **overrides):
    """Loads the saved display configuration every scenario starts from.

    Callers add their own routes, flags and overrides, then PUT it themselves.
    """
    config = service.request('/config')['config']
    config.update(repository=str(service.root / 'checkout'), github_repo='fixture/project', verification_commands=commands, session_timeout_seconds=30, command_timeout_seconds=10, **overrides)
    return config


def use_codex_routes(config):
    """Routes every role, tier and the repair route to CODEX_ROUTE, each a copy."""
    for role in config['roles']:
        config['roles'][role] = dict(CODEX_ROUTE)
    for tier in config['tiers']:
        config['tiers'][tier] = dict(CODEX_ROUTE)
    config['repair_route'] = dict(CODEX_ROUTE)


def route(backend, planning=False, provider='fixture', variant='high'):
    if backend == 'codex':
        return dict(CODEX_ROUTE)
    return {'backend': 'opencode', 'provider': provider, 'model': 'plain-model' if planning else 'fixture-model', 'effort': '', **({'variant': variant} if variant and not planning else {})}


def configuration(service, planning='opencode', executor='opencode', reviewer='opencode', repair='opencode'):
    """Saves a mixed-runner configuration, OpenCode everywhere by default, and returns it."""
    c = base_config(service, ['test "$(cat feature.txt)" = fixed'], task_timeout_seconds=120)
    for role in ['orchestrator', 'discovery', 'proposal_reviewer']:
        c['roles'][role] = route(planning, planning=True)
    c['roles']['code_reviewer'] = route(reviewer)
    c['tiers'] = {tier: route(executor) for tier in c['tiers']}
    c['repair_route'] = route(repair, provider='alternate', variant=None)
    if {planning, executor, reviewer, repair} == {'opencode'}:
        c['codex_binary'] = '/codex-is-not-installed'
    service.save_config(c)
    return c


class Service:
    def __init__(self, root):
        self.root = root
        self.process = None
        self.race_reported = False
        self.log = (root / 'service.log').open('a')
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            self.port = sock.getsockname()[1]
        self.env = {key: value for key, value in os.environ.items() if key != 'OCTOMUS_NOTIFICATION_WEBHOOK_URL'}
        # Fixture runners are host scripts; tests/e2e_sandbox.py covers the Docker sandbox.
        self.env.update({'OCTOMUS_TOKEN': TOKEN, 'OCTOMUS_FIXTURE': str(root), 'OCTOMUS_SANDBOX': 'off', 'PATH': f'{root / "bin"}:{os.environ["PATH"]}'})

    def start(self):
        self.process = subprocess.Popen([str(BINARY), '--data-dir', str(self.root / '.octomus'), '--listen', f'127.0.0.1:{self.port}', '--assets', str(PROJECT / 'web/build')], env=self.env, stdout=self.log, stderr=self.log)
        self.wait(lambda: self.request('/healthz', api=False), 'service startup')

    def stop(self, crash=False):
        if self.process and self.process.poll() is None:
            self.process.kill() if crash else self.process.terminate()
            try:
                self.process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
                raise AssertionError(f'service did not stop within 15s of {"SIGKILL" if crash else "SIGTERM"}; service.log tail:\n{service_log(self.root, tail=100)}')
        if self.process and self.process.returncode == RACE_EXIT_STATUS and not self.race_reported:
            self.race_reported = True
            raise AssertionError(f'service exited with status {RACE_EXIT_STATUS}: the race detector reported a data race; service.log tail:\n{service_log(self.root, tail=200)}')

    def _open(self, path, method, value, api, timeout):
        """Sends one authenticated request; `timeout` bounds each socket operation,
        so a response the service holds longer than that raises TimeoutError."""
        request = urllib.request.Request(f'http://127.0.0.1:{self.port}{"/api" if api else ""}{path}', method=method, headers={'Authorization': f'Bearer {TOKEN}', 'Content-Type': 'application/json'}, data=json.dumps(value or {}).encode() if method != 'GET' else None)
        return urllib.request.urlopen(request, timeout=timeout)

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

    def configure(self):
        commands = ['false'] if (self.root / 'failed-verification').exists() else ['for file in feature*.txt; do test "$(cat "$file")" = fixed || exit 1; done']
        config = base_config(self, commands, cycle_interval_seconds=3600, task_timeout_seconds=120)
        use_codex_routes(config)
        if (self.root / 'custom-route').exists():
            config['repair_route'] = {'backend': 'codex', 'model': 'gpt-5.6-luna', 'effort': 'high'}
            config['tiers']['M'] = {'model': 'gpt-5.6-luna', 'effort': 'low'}
        if (self.root / 'cap1-interrupt').exists():
            config['max_open_prs'] = 1
        self.save_config(config)
        diagnostic = self.request('/doctor', 'POST')
        view = self.request('/config')
        assert diagnostic['checked_revision'] == view['revision']
        assert diagnostic['checked_config'] == view['config']
        assert diagnostic['codex_version'] == 'codex-cli 0.153.4'
        assert diagnostic['tested_codex_version'] == '0.153.4' and diagnostic['warnings'] == []
        (self.root / 'version').write_text('0.0.0-fixture')
        diagnostic = self.request('/doctor', 'POST')
        assert 'mismatch' in diagnostic['message'] and len(diagnostic['warnings']) == 1
        (self.root / 'version').unlink()
        self.request('/control/cycle', 'POST')

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
    for helper in ['worker.py', 'git_rollback.py']:
        shutil.copy(PROJECT / 'tests/fixtures' / helper, root / 'bin' / helper)
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


def existing_pr(root):
    checkout = root / 'checkout'
    git('checkout', '-b', 'octomus/existing', cwd=checkout)
    (checkout / 'earlier.txt').write_text('Preserve the earlier improvement.\n')
    git('add', '.', cwd=checkout)
    git('commit', '-m', 'Earlier Octomus work', cwd=checkout)
    git('push', 'origin', 'octomus/existing', cwd=checkout)
    head = git('rev-parse', 'HEAD', cwd=checkout)
    git('checkout', 'main', cwd=checkout)
    (root / 'target').write_text('octomus/existing')
    (root / 'prs.json').write_text(json.dumps([{'number': 42, 'title': 'An existing improvement', 'body': 'Existing context.\n<!-- octomus:task:earlier -->', 'head': {'ref': 'octomus/existing', 'sha': head, 'repo': {'full_name': 'fixture/project'}}, 'base': {'ref': 'main'}, 'html_url': 'https://github.com/fixture/project/pull/42', 'state': 'open', 'merged_at': None, 'additions': 2000, 'deletions': 0, 'created_at': '2026-08-01T00:00:00Z'}]))


def update_prs(root, change):
    """Lets `change(prs)` edit the gh fixture's saved PR list.

    It holds the fixture's github.lock, as every gh invocation does, and
    replaces prs.json whole, so a background gh read never sees a truncated
    file. Never call it while root/reconcile-hold exists: a held `gh auth
    status` keeps the lock until the hold is released.
    """
    with (root / 'github.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        path = root / 'prs.json'
        prs = json.loads(path.read_text())
        change(prs)
        temporary = root / 'prs.json.tmp'
        temporary.write_text(json.dumps(prs))
        os.replace(temporary, path)


def usage_report(root):
    report = json.loads(subprocess.check_output([str(BINARY), '--data-dir', str(root / '.octomus'), '--usage-report'], text=True, timeout=30))
    assert sum(d['admissions'] for d in report['daily']) == len(report['admissions'])
    assert all(d['unattributed_admissions'] == 0 for d in report['daily'])
    return report


def stop_peers(root):
    """Kills the OpenCode peer process groups a scenario left behind."""
    path = root / 'opencode-pids.jsonl'
    pids = [json.loads(row)['pid'] for row in path.read_text().splitlines()] if path.exists() else []
    child = root / 'opencode-child-pid'
    if child.exists():
        pids.append(int(child.read_text()))
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
