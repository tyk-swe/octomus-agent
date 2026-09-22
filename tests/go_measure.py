#!/usr/bin/env python3
"""M8 measurement driver: build times, idle RSS and /api/state latency at scale.

Implements the measurement protocol frozen in docs/roadmap/m8-qualification.md:
same host, fixed fixture data, equivalent optimized builds and the same prebuilt
frontend for the frozen Rust reference and the Go candidate.

For each scale (default 1,000 / 10,000 / 100,000 historical task records) the
driver creates an isolated data directory, initializes the schema by booting the
binary under test once, seeds task rows in one transaction (the record shape
from tests/history_scale.rs: a published task whose proposal carries a ~4 KiB
prompt), restarts the service paused/idle on a free loopback port, warms up,
then times `requests` sequential GET /api/state calls over one keep-alive
connection. Idle RSS is sampled from /proc/<pid>/status VmRSS.

Builds: the Rust clean build runs `cargo build --release --locked` in a pristine
source copy (never the checked-out worktrees) after an untimed `cargo fetch`, so
dependency downloads are separated from compilation. The Go clean build runs
`CGO_ENABLED=0 go build -trimpath` with a fresh GOCACHE and the shared module
cache. Incremental builds append a marker comment to one backend source file
(src/engine.rs in the copy, internal/engine/engine.go here) and rebuild, median
of three; the Go file is restored afterwards.

Environment: OCTOMUS_RUST_BIN / OCTOMUS_GO_BIN name the executables (falling
back to the repo-conventional OCTOMUS_RUST_REFERENCE / OCTOMUS_TEST_BINARY, then
target/release/octomus-agent and bin/octomus-agent). OCTOMUS_RUST_SRC names
the Rust source tree to copy for build measurements (default: this worktree).

Usage:
    python3 tests/go_measure.py --rust-bin <path> --go-bin <path> --out report.md
"""
import argparse
import http.client
import json
import math
import os
import shutil
import socket
import sqlite3
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

PROJECT = Path(__file__).resolve().parents[1]
TOKEN = 'fixture-operator-token-with-at-least-32-characters'
STATE_JSON = 'state.db'
TASK_PROMPT = 'x' * 4096
# Budgets frozen by the M8 qualification milestone; never relax them silently.
P95_GROWTH_BUDGET = 2.0
SIZE_GROWTH_BUDGET = 1.05
P95_FLOOR_MS = 50.0


def clean_env():
    """Drops every OCTOMUS_* override so fixture env never leaks into a run."""
    return {key: value for key, value in os.environ.items() if not key.startswith('OCTOMUS_')}


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def poll(predicate, seconds, interval=0.1, tick=None):
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


def nearest_rank(sorted_values, fraction):
    """Nearest-rank percentile: sorted[ceil(f * n) - 1], f in (0, 1]."""
    return sorted_values[max(0, math.ceil(fraction * len(sorted_values)) - 1)]


def vmrss_kib(pid):
    with open(f'/proc/{pid}/status') as status:
        for line in status:
            if line.startswith('VmRSS:'):
                return int(line.split()[1])
    return None


def dir_bytes(path):
    total = 0
    for entry in Path(path).rglob('*'):
        if entry.is_file() and not entry.is_symlink():
            total += entry.stat().st_size
    return total


class Service:
    """One paused service instance: real binary, real SQLite, loopback HTTP."""

    def __init__(self, binary, data_dir, assets, log):
        self.binary = binary
        self.data_dir = Path(data_dir)
        self.port = free_port()
        self.log = open(log, 'a')
        env = clean_env()
        env['OCTOMUS_TOKEN'] = TOKEN
        self.process = subprocess.Popen(
            [str(binary), '--data-dir', str(self.data_dir), '--listen', f'127.0.0.1:{self.port}', '--assets', str(assets)],
            env=env, stdout=self.log, stderr=self.log)

    def wait_ready(self, seconds=120):
        def ready():
            with urllib.request.urlopen(f'http://127.0.0.1:{self.port}/healthz', timeout=2) as response:
                return response.status == 200

        def exited():
            if self.process.poll() is not None:
                raise AssertionError(f'{self.binary.name} exited during startup; log: {self.log.name}')

        if not poll(ready, seconds, tick=exited):
            raise AssertionError(f'{self.binary.name} did not answer /healthz within {seconds}s; log: {self.log.name}')

    def request(self, path, method='GET'):
        request = urllib.request.Request(
            f'http://127.0.0.1:{self.port}/api{path}', method=method,
            headers={'Authorization': f'Bearer {TOKEN}', 'Content-Type': 'application/json'},
            data=b'{}' if method != 'GET' else None)
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)

    def rss_kib(self):
        if self.process.poll() is not None:
            return None
        return vmrss_kib(self.process.pid)

    def stop(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=15)
        self.log.close()


def initialize_schema(binary, assets, work_dir, label):
    """Boots the binary once on an empty data dir so it creates and migrates
    state.db, then checkpoints WAL so the file can be copied as a template."""
    data_dir = work_dir / f'{label}-schema' / 'data'
    data_dir.mkdir(parents=True)
    service = Service(binary, data_dir, assets, work_dir / f'{label}-schema.log')
    try:
        service.wait_ready()
    finally:
        service.stop()
    db = data_dir / STATE_JSON
    with sqlite3.connect(db) as conn:
        conn.execute('PRAGMA wal_checkpoint(TRUNCATE)')
    return db


TASK_TEMPLATE = {
    'status': 'published',
    'proposal': {'title': 'Historical task', 'target': 'main', 'prompt': TASK_PROMPT},
}


def seed_tasks(db_path, count):
    """Inserts `count` published task records in one transaction; the service's
    own triggers project them into record_meta and record_counts. Timestamps
    stay inside the 14-day retention window so housekeeping never discards the
    fixture mid-measurement."""
    started = time.monotonic()
    base = time.time()
    conn = sqlite3.connect(db_path)
    try:
        with conn:  # one transaction: triggers project meta and counts once
            batch = []
            for i in range(count):
                stamp = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime(base - (count - i)))
                row = dict(TASK_TEMPLATE)
                row['id'] = f'historical-{i}'
                row['created_at'] = stamp
                row['updated_at'] = stamp
                batch.append((row['id'], json.dumps(row, separators=(',', ':'))))
                if len(batch) >= 5000:
                    conn.executemany("INSERT INTO records VALUES ('task',?1,?2)", batch)
                    batch.clear()
            if batch:
                conn.executemany("INSERT INTO records VALUES ('task',?1,?2)", batch)
        conn.execute('PRAGMA wal_checkpoint(TRUNCATE)')
    finally:
        conn.close()
    return time.monotonic() - started


def measure_state(label, binary, assets, scale, template_db, work_dir, requests, warmup, settle):
    """Seeds one isolated copy of the schema template, serves it paused and
    times sequential GET /api/state calls on a single keep-alive connection."""
    run_dir = work_dir / f'state-{label}-{scale}'
    data_dir = run_dir / 'data'
    data_dir.mkdir(parents=True)
    shutil.copyfile(template_db, data_dir / STATE_JSON)
    seed_seconds = seed_tasks(data_dir / STATE_JSON, scale)
    db_bytes = dir_bytes(data_dir)

    service = Service(binary, data_dir, assets, run_dir / 'service.log')
    try:
        service.wait_ready()
        time.sleep(settle)
        state = service.request('/state')
        control = state['control']
        if not control['paused']:
            service.request('/control/pause', 'POST')
            state = service.request('/state')
            control = state['control']
        assert control['paused'], f'{binary.name} is not paused: {control}'
        published = state['counts'].get('published', 0)
        assert published == scale, f'{binary.name} sees {published} published tasks, expected {scale}'

        connection = http.client.HTTPConnection('127.0.0.1', service.port, timeout=30)
        headers = {'Authorization': f'Bearer {TOKEN}'}
        try:
            for _ in range(warmup):
                connection.request('GET', '/api/state', headers=headers)
                response = connection.getresponse()
                response.read()
                assert response.status == 200, response.status
            rss_after_warmup = service.rss_kib()
            elapsed_ms, sizes = [], []
            for _ in range(requests):
                start = time.monotonic()
                connection.request('GET', '/api/state', headers=headers)
                response = connection.getresponse()
                body = response.read()
                elapsed_ms.append((time.monotonic() - start) * 1000)
                assert response.status == 200, response.status
                sizes.append(len(body))
        finally:
            connection.close()
        time.sleep(0.5)
        rss_after_requests = service.rss_kib()
    finally:
        service.stop()

    ordered = sorted(elapsed_ms)
    return {
        'scale': scale,
        'seed_seconds': seed_seconds,
        'db_bytes': db_bytes,
        'requests': len(elapsed_ms),
        'warmup': warmup,
        'p50_ms': nearest_rank(ordered, 0.50),
        'p95_ms': nearest_rank(ordered, 0.95),
        'min_ms': ordered[0],
        'max_ms': ordered[-1],
        'mean_ms': statistics.fmean(elapsed_ms),
        'response_bytes': sizes[0],
        'response_bytes_max': max(sizes),
        'idle_rss_kib_after_warmup': rss_after_warmup,
        'idle_rss_kib_after_requests': rss_after_requests,
        'samples_ms': elapsed_ms,
    }


def timed(argv, cwd, env=None):
    merged = dict(os.environ)
    if env:
        merged.update(env)
    start = time.monotonic()
    subprocess.run(argv, cwd=cwd, env=merged, check=True)
    return time.monotonic() - start


def copy_source_tree(src, dst):
    """Pristine Rust source copy: no build outputs, VCS metadata or vendored
    dependencies, so clean-build timing never touches the worktrees."""
    ignore = shutil.ignore_patterns('target', '.git', 'node_modules', 'dist', 'bin', '*.log', 'service.lock', 'state.db*')
    shutil.copytree(src, dst, ignore=ignore)


def measure_builds(rust_src, go_src, work_dir):
    """Clean and incremental build timings for both toolchains.

    Rust builds happen entirely inside a throwaway copy of the source tree.
    Go clean build uses a fresh GOCACHE (module downloads stay cached). Each
    incremental sample appends a marker comment to one backend file, rebuilds
    and restores it; content changes are required because the Go build cache
    keys on content, and they keep the Rust fingerprint honest too."""
    builds = {}

    rust_copy = work_dir / 'rust-src-copy'
    copy_source_tree(rust_src, rust_copy)
    subprocess.run(['cargo', 'fetch', '--locked'], cwd=rust_copy, check=True)
    try:
        builds['rust_clean_s'] = timed(['cargo', 'build', '--release', '--locked', '--offline'], cwd=rust_copy)
    except subprocess.CalledProcessError:
        print('offline cargo build failed; falling back to online (downloads not separated)', flush=True)
        builds['rust_clean_s'] = timed(['cargo', 'build', '--release', '--locked'], cwd=rust_copy)
        builds['rust_offline'] = False
    else:
        builds['rust_offline'] = True

    rust_file = rust_copy / 'src/engine.rs'
    rust_original = rust_file.read_bytes()
    samples = []
    try:
        for i in range(3):
            rust_file.write_bytes(rust_original + f'\n// m8 measurement probe {i}\n'.encode())
            samples.append(timed(['cargo', 'build', '--release', '--locked', '--offline'], cwd=rust_copy))
    finally:
        rust_file.write_bytes(rust_original)
    builds['rust_incremental_samples_s'] = samples
    builds['rust_incremental_median_s'] = statistics.median(samples)

    go_cache = work_dir / 'gocache'
    go_cache.mkdir(parents=True, exist_ok=True)
    go_env = {'CGO_ENABLED': '0', 'GOCACHE': str(go_cache)}
    go_out = work_dir / 'octomus-agent-go-measured'
    builds['go_clean_s'] = timed(
        ['go', 'build', '-trimpath', '-o', str(go_out), './cmd/octomus-agent'], cwd=go_src, env=go_env)

    go_file = go_src / 'internal/engine/engine.go'
    go_original = go_file.read_bytes()
    samples = []
    try:
        for i in range(3):
            go_file.write_bytes(go_original + f'\n// m8 measurement probe {i}\n'.encode())
            samples.append(timed(
                ['go', 'build', '-trimpath', '-o', str(go_out), './cmd/octomus-agent'], cwd=go_src, env=go_env))
    finally:
        go_file.write_bytes(go_original)
    builds['go_incremental_samples_s'] = samples
    builds['go_incremental_median_s'] = statistics.median(samples)
    return builds


def toolchains():
    def version(argv):
        try:
            return subprocess.check_output(argv, text=True, stderr=subprocess.DEVNULL).strip()
        except (OSError, subprocess.CalledProcessError):
            return 'unavailable'

    cpu = 'unknown'
    with open('/proc/cpuinfo') as info:
        for line in info:
            if line.startswith('model name'):
                cpu = line.split(':', 1)[1].strip()
                break
    mem_kib = 0
    with open('/proc/meminfo') as info:
        for line in info:
            if line.startswith('MemTotal:'):
                mem_kib = int(line.split()[1])
    return {
        'date_utc': time.strftime('%Y-%m-%d %H:%M:%SZ', time.gmtime()),
        'kernel': subprocess.check_output(['uname', '-srvm'], text=True).strip(),
        'cpu': cpu,
        'cores': os.cpu_count(),
        'mem_gib': round(mem_kib / 1024 / 1024, 1),
        'loadavg': os.getloadavg(),
        'rustc': version(['rustc', '--version']),
        'cargo': version(['cargo', '--version']),
        'go': version(['go', 'version']),
        'python': version([sys.executable, '--version']),
    }


def provenance(binary, src):
    """Binary identity: sha256 plus the source tree's HEAD, for a report that
    can be tied back to exact revisions."""
    import hashlib
    digest = hashlib.sha256(Path(binary).read_bytes()).hexdigest()
    try:
        revision = subprocess.check_output(['git', '-C', str(src), 'rev-parse', 'HEAD'], text=True).strip()
    except (OSError, subprocess.CalledProcessError):
        revision = 'unknown'
    return {'sha256': digest, 'source_revision': revision}


def fmt_ms(value):
    return f'{value:.2f}'


def fmt_mib(kib):
    return f'{kib / 1024:.1f}' if kib is not None else 'n/a'


def render_report(args, meta, builds, runs, budgets):
    """Markdown report: protocol details, per-scale table, budgets PASS/FAIL."""
    lines = [
        '# M8 measurement record — build and /api/state scale',
        '',
        f"Date (UTC): {meta['date_utc']}",
        '',
        '## Protocol (frozen in docs/roadmap/m8-qualification.md)',
        '',
        f"- Host: {meta['kernel']}, {meta['cpu']}, {meta['cores']} cores, {meta['mem_gib']} GiB RAM, loadavg {meta['loadavg'][0]:.2f}/{meta['loadavg'][1]:.2f}/{meta['loadavg'][2]:.2f} at run start",
        f"- Toolchains: {meta['rustc']}; {meta['cargo']}; {meta['go']}; {meta['python']}",
        f"- Rust binary: `{args.rust_bin}` (sha256 {meta['rust']['sha256'][:16]}…, source {meta['rust']['source_revision']})",
        f"- Go binary: `{args.go_bin}` (sha256 {meta['go']['sha256'][:16]}…, source {meta['go']['source_revision']})",
        '- Rust build: `cargo build --release --locked --offline` in a pristine source copy after `cargo fetch --locked` (dependency download separated from compilation); incremental = append a marker comment to `src/engine.rs`, rebuild, restore, median of 3',
        '- Go build: `CGO_ENABLED=0 go build -trimpath -o <out> ./cmd/octomus-agent` with a fresh GOCACHE and the shared module cache; incremental = append a marker comment to `internal/engine/engine.go`, rebuild, restore, median of 3',
        f"- Frontend: identical prebuilt dashboard served via `--assets {args.assets}`",
        f"- Fixture: schema initialized by each binary on a fresh data dir, then `INSERT INTO records VALUES ('task', id, json)` in one transaction; published task, ~4 KiB `proposal.prompt`, timestamps inside the 14-day retention window",
        f"- Service: `--data-dir <isolated> --listen 127.0.0.1:<free port>`, `OCTOMUS_TOKEN` fixture token, boots paused (verified via control.paused before measuring)",
        f"- Latency: {args.warmup} warmup + {args.requests} timed sequential `GET /api/state` requests on one keep-alive HTTP/1.1 connection, client-side wall clock (`time.monotonic`); percentiles are nearest-rank",
        f"- Idle RSS: `VmRSS` from `/proc/<pid>/status` after warmup and again after the timed run",
        f"- Settle after startup: {args.settle}s",
        '',
        '## Build times',
        '',
        '| Metric | Rust | Go |',
        '| --- | ---: | ---: |',
    ]
    if builds:
        rust_inc = ', '.join(f'{s:.2f}' for s in builds['rust_incremental_samples_s'])
        go_inc = ', '.join(f'{s:.2f}' for s in builds['go_incremental_samples_s'])
        lines += [
            f"| Clean build (s) | {builds['rust_clean_s']:.1f} | {builds['go_clean_s']:.1f} |",
            f"| Incremental median (s) | {builds['rust_incremental_median_s']:.2f} | {builds['go_incremental_median_s']:.2f} |",
            f"| Incremental samples (s) | {rust_inc} | {go_inc} |",
        ]
        ratio = builds['rust_incremental_median_s'] / builds['go_incremental_median_s']
        lines += [
            '',
            f"Incremental-build improvement (Rust median / Go median): **{ratio:.1f}×** "
            '(informational target per the milestone; not a numeric budget).',
        ]
    else:
        lines.append('| (skipped) | | |')
    lines += [
        '',
        '## /api/state at scale',
        '',
        '| Tasks | Impl | p50 ms | p95 ms | max ms | Response B | Idle RSS MiB (post-warmup / post-run) | Seed s | DB MiB |',
        '| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |',
    ]
    for scale in args.scales:
        for name in ['rust', 'go']:
            if scale not in runs.get(name, {}):
                continue
            r = runs[name][scale]
            lines.append(
                f"| {scale} | {name} | {fmt_ms(r['p50_ms'])} | {fmt_ms(r['p95_ms'])} | {fmt_ms(r['max_ms'])} "
                f"| {r['response_bytes']} | {fmt_mib(r['idle_rss_kib_after_warmup'])} / {fmt_mib(r['idle_rss_kib_after_requests'])} "
                f"| {r['seed_seconds']:.1f} | {r['db_bytes'] / 1024 / 1024:.0f} |")
    lines += ['', '## Budget evaluation', '']
    for entry in budgets:
        lines.append(f"- {'PASS' if entry['ok'] else 'FAIL'} — {entry['text']}")
    overall = all(entry['ok'] for entry in budgets)
    lines += [
        '',
        f"**Overall: {'PASS' if overall else 'FAIL'}**" if budgets else '**Overall: not evaluated (services skipped)**',
        '',
        f"Raw samples and environment: `{Path(args.out).name}.json` next to this report.",
        '',
    ]
    return '\n'.join(lines)


def evaluate(runs, args):
    scales = sorted(args.scales)
    lo, hi = scales[0], scales[-1]
    budgets = []
    rust_p95 = runs['rust'][hi]['p95_ms']
    go_p95 = runs['go'][hi]['p95_ms']
    ceiling = max(2 * rust_p95, P95_FLOOR_MS)
    budgets.append({
        'ok': go_p95 <= ceiling,
        'text': f'At {hi} tasks, Go p95 {go_p95:.2f} ms <= max(2 x Rust p95 {rust_p95:.2f} ms, {P95_FLOOR_MS:.0f} ms) = {ceiling:.2f} ms',
    })
    growth = runs['go'][hi]['p95_ms'] / runs['go'][lo]['p95_ms']
    budgets.append({
        'ok': growth <= P95_GROWTH_BUDGET,
        'text': f'Go p95 growth {lo} -> {hi}: {runs["go"][lo]["p95_ms"]:.2f} -> {runs["go"][hi]["p95_ms"]:.2f} ms = {growth:.2f}x <= {P95_GROWTH_BUDGET:.1f}x',
    })
    for name in ['go', 'rust']:
        size_growth = runs[name][hi]['response_bytes'] / runs[name][lo]['response_bytes']
        budgets.append({
            'ok': (size_growth <= SIZE_GROWTH_BUDGET) or name == 'rust',
            'text': f'{name.capitalize()} response-size growth {lo} -> {hi}: {runs[name][lo]["response_bytes"]} -> {runs[name][hi]["response_bytes"]} B = {(size_growth - 1) * 100:.2f}% <= {int((SIZE_GROWTH_BUDGET - 1) * 100)}%' + ('' if name == 'go' else ' (informational; budget applies to Go)'),
        })
    return budgets


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--rust-bin', default=os.environ.get('OCTOMUS_RUST_BIN') or os.environ.get('OCTOMUS_RUST_REFERENCE') or str(PROJECT / 'target/release/octomus-agent'))
    parser.add_argument('--go-bin', default=os.environ.get('OCTOMUS_GO_BIN') or os.environ.get('OCTOMUS_TEST_BINARY') or str(PROJECT / 'bin/octomus-agent'))
    parser.add_argument('--rust-src', default=os.environ.get('OCTOMUS_RUST_SRC', str(PROJECT)), help='Rust source tree copied for build measurements')
    parser.add_argument('--go-src', default=str(PROJECT), help='Go module root for build measurements')
    parser.add_argument('--assets', default=str(PROJECT / 'web/build'), help='Prebuilt dashboard directory passed to both services')
    parser.add_argument('--scales', default='1000,10000,100000', help='Comma-separated task counts, ascending')
    parser.add_argument('--requests', type=int, default=50)
    parser.add_argument('--warmup', type=int, default=15)
    parser.add_argument('--settle', type=float, default=2.0, help='Seconds between /healthz and warmup')
    parser.add_argument('--work-dir', default=None, help='Scratch area (default: a temp dir removed on success)')
    parser.add_argument('--keep-work', action='store_true')
    parser.add_argument('--skip-builds', action='store_true')
    parser.add_argument('--skip-services', action='store_true')
    parser.add_argument('--out', required=True, help='Markdown report path; raw JSON lands at <out>.json')
    args = parser.parse_args()

    args.rust_bin = Path(args.rust_bin).resolve()
    args.go_bin = Path(args.go_bin).resolve()
    args.scales = [int(s) for s in args.scales.split(',')]
    assert args.scales == sorted(args.scales), '--scales must be ascending'
    for binary in [args.rust_bin, args.go_bin]:
        if not binary.is_file():
            parser.error(f'missing binary: {binary}')
    if not (Path(args.assets) / '200.html').is_file():
        parser.error(f'--assets {args.assets} lacks 200.html; build the dashboard first')

    work_root = Path(args.work_dir) if args.work_dir else Path(tempfile.mkdtemp(prefix='octomus-m8-'))
    work_root.mkdir(parents=True, exist_ok=True)
    work_dir = Path(tempfile.mkdtemp(prefix='run-', dir=work_root))
    print(f'work dir: {work_dir}', flush=True)

    meta = toolchains()
    meta['rust'] = provenance(args.rust_bin, args.rust_src)
    meta['go'] = provenance(args.go_bin, args.go_src)
    builds = None
    if not args.skip_builds:
        print('== builds ==', flush=True)
        builds = measure_builds(Path(args.rust_src), Path(args.go_src), work_dir)
        print(json.dumps(builds, indent=2), flush=True)

    runs = {'rust': {}, 'go': {}}
    if not args.skip_services:
        templates = {}
        for name, binary in [('rust', args.rust_bin), ('go', args.go_bin)]:
            templates[name] = initialize_schema(binary, args.assets, work_dir, name)
        # Interleave implementations inside each scale so host load that drifts
        # over the run affects both sides at the same fixture size.
        for scale in args.scales:
            for name, binary in [('rust', args.rust_bin), ('go', args.go_bin)]:
                print(f'== {name} {scale} ==', flush=True)
                result = measure_state(name, binary, args.assets, scale, templates[name], work_dir, args.requests, args.warmup, args.settle)
                runs[name][scale] = result
                print(f"  p50={result['p50_ms']:.2f}ms p95={result['p95_ms']:.2f}ms max={result['max_ms']:.2f}ms "
                      f"bytes={result['response_bytes']} rss={fmt_mib(result['idle_rss_kib_after_requests'])}MiB "
                      f"seed={result['seed_seconds']:.1f}s", flush=True)

    budgets = evaluate(runs, args) if not args.skip_services else []
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    report = render_report(args, meta, builds, runs, budgets)
    out.write_text(report)
    raw = {name: {str(scale): {k: v for k, v in r.items()} for scale, r in by_scale.items()} for name, by_scale in runs.items()}
    Path(str(out) + '.json').write_text(json.dumps({'meta': meta, 'args': {k: str(v) for k, v in vars(args).items()}, 'builds': builds, 'runs': raw, 'budgets': budgets}, indent=2))
    print(report, flush=True)
    if not args.keep_work and not args.work_dir:
        shutil.rmtree(work_dir, ignore_errors=True)
    return 0 if all(b['ok'] for b in budgets) else 1


if __name__ == '__main__':
    sys.exit(main())
