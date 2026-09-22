#!/usr/bin/env python3
"""M8 measurement driver: build times, idle RSS and /api/state latency at scale.

Implements the measurement protocol frozen in docs/roadmap/
m0-reference-and-contracts.md (recorded before Go results were known): same
otherwise-idle Linux amd64 host, fixed fixture data, equivalent optimized
builds and the same prebuilt frontend for the frozen Rust reference and the Go
candidate.

For each scale (default 1,000 / 10,000 / 100,000 historical task records) the
driver creates an isolated data directory, initializes the schema by booting the
binary under test once, seeds task rows in one transaction (the record shape
from tests/history_scale.rs: a published task whose proposal carries a ~4 KiB
prompt), then for each of `runs` fresh service runs boots paused/idle on a free
loopback port, samples idle VmRSS after `settle` seconds, warms up `warmup`
requests, and times `requests` sequential GET /api/state calls over one
keep-alive connection. p50/p95 are nearest-rank over the pooled samples of all
five runs; per-run p95s are also reported.

Builds: Rust clean builds run `cargo build --release --locked` in a pristine
source copy (never the checked-out worktrees) after an untimed `cargo fetch`,
each with a fresh CARGO_TARGET_DIR so dependency downloads are separated from
compilation. Go clean builds run `CGO_ENABLED=0 go build -trimpath
-ldflags=-s -w`, each with a fresh GOCACHE and the shared module cache.
Incremental builds apply one function-body literal change (a log/status string)
to one backend source file (src/engine.rs in the copy, internal/engine/
engine.go here), rebuild with warm caches, and restore — one discarded warmup
then `incrementals` timed samples; medians are reported.

Environment: OCTOMUS_RUST_BIN / OCTOMUS_GO_BIN name the executables (falling
back to the repo-conventional OCTOMUS_RUST_REFERENCE / OCTOMUS_TEST_BINARY, then
target/release/octomus-agent and bin/octomus-agent). OCTOMUS_RUST_SRC names
the Rust source tree to copy for build measurements (default: this worktree).

Usage:
    python3 tests/go_measure.py --rust-bin <path> --go-bin <path> --out report.md
"""
import argparse
import hashlib
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
sys.path.insert(0, str(PROJECT / 'tests'))
from e2e import poll, TOKEN  # noqa: E402
from go_storage import clean_env, copy_state  # noqa: E402

STATE_JSON = 'state.db'
TASK_PROMPT = 'x' * 4096
# Budgets frozen by the M8 qualification milestone; never relax them silently.
P95_GROWTH_BUDGET = 2.0
SIZE_GROWTH_BUDGET = 1.05
P95_FLOOR_MS = 50.0
# Sample counts frozen by the M0 measurement protocol.
CLEAN_BUILDS = 5
INCREMENTAL_BUILDS = 10
SERVICE_RUNS = 5
WARMUP_REQUESTS = 100
TIMED_REQUESTS = 1000
IDLE_SETTLE_S = 30.0


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


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


def measure_state(label, binary, assets, scale, template_db, work_dir, requests, warmup, runs, settle):
    """Seeds one fixture, then repeats the frozen protocol in `runs` fresh
    service runs against identical copies: idle settle + VmRSS sample, `warmup`
    warmup requests, `requests` timed sequential GET /api/state calls on one
    keep-alive connection each. Returns pooled percentiles plus per-run p95s."""
    fixture_dir = work_dir / f'state-{label}-{scale}'
    fixture_data = fixture_dir / 'data'
    fixture_data.mkdir(parents=True)
    shutil.copyfile(template_db, fixture_data / STATE_JSON)
    seed_seconds = seed_tasks(fixture_data / STATE_JSON, scale)
    db_bytes = dir_bytes(fixture_data)

    elapsed_ms, sizes, per_run_p95, idle_rss = [], [], [], []
    for run in range(runs):
        run_dir = work_dir / f'state-{label}-{scale}-run{run}'
        data_dir = copy_state(fixture_data, run_dir / 'data')

        service = Service(binary, data_dir, assets, run_dir / 'service.log')
        try:
            service.wait_ready()
            state = service.request('/state')
            control = state['control']
            if not control['paused']:
                service.request('/control/pause', 'POST')
                state = service.request('/state')
                control = state['control']
            assert control['paused'], f'{binary.name} is not paused: {control}'
            if run == 0:
                published = state['counts'].get('published', 0)
                assert published == scale, f'{binary.name} sees {published} published tasks, expected {scale}'
            time.sleep(settle)
            idle_rss.append(service.rss_kib())

            connection = http.client.HTTPConnection('127.0.0.1', service.port, timeout=30)
            headers = {'Authorization': f'Bearer {TOKEN}'}
            try:
                for _ in range(warmup):
                    connection.request('GET', '/api/state', headers=headers)
                    response = connection.getresponse()
                    response.read()
                    assert response.status == 200, response.status
                run_ms = []
                for _ in range(requests):
                    start = time.monotonic()
                    connection.request('GET', '/api/state', headers=headers)
                    response = connection.getresponse()
                    body = response.read()
                    elapsed_ms.append((time.monotonic() - start) * 1000)
                    run_ms.append(elapsed_ms[-1])
                    assert response.status == 200, response.status
                    sizes.append(len(body))
            finally:
                connection.close()
            per_run_p95.append(nearest_rank(sorted(run_ms), 0.95))
        finally:
            service.stop()

    ordered = sorted(elapsed_ms)
    assert len(set(sizes)) == 1, f'{binary.name} response size varied across {runs} runs: {sorted(set(sizes))}'
    return {
        'scale': scale,
        'seed_seconds': seed_seconds,
        'db_bytes': db_bytes,
        'runs': runs,
        'requests': len(elapsed_ms),
        'warmup': warmup,
        'p50_ms': nearest_rank(ordered, 0.50),
        'p95_ms': nearest_rank(ordered, 0.95),
        'min_ms': ordered[0],
        'max_ms': ordered[-1],
        'mean_ms': statistics.fmean(elapsed_ms),
        'per_run_p95_ms': per_run_p95,
        'response_bytes': sizes[0],
        'idle_rss_kib_median': statistics.median(v for v in idle_rss if v is not None),
        'idle_rss_kib_samples': idle_rss,
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


def with_literal_probe(path, anchor, index):
    """Applies one function-body literal change: rewrites the first occurrence
    of `anchor` (a string literal) to carry the probe index inside the string."""
    assert isinstance(anchor, bytes) and anchor.startswith(b'"') and anchor.endswith(b'"')
    original = path.read_bytes()
    probe = anchor[:-1] + str(index).encode() + anchor[-1:]
    applied = original.replace(anchor, probe, 1)
    assert applied != original, f'literal anchor {anchor} not found in {path}'
    path.write_bytes(applied)


def measure_builds(rust_src, go_src, work_dir, clean_count, incremental_count):
    """Frozen-protocol build timings: `clean_count` clean builds per toolchain
    against fresh target/cache directories, then one discarded warmup plus
    `incremental_count` timed incremental builds after a function-body literal
    change, medians reported. Rust builds run inside a throwaway source copy
    (fresh CARGO_TARGET_DIR each time); Go builds use a fresh GOCACHE each time
    (module downloads stay in the shared module cache)."""
    builds = {}

    rust_copy = work_dir / 'rust-src-copy'
    copy_source_tree(rust_src, rust_copy)
    subprocess.run(['cargo', 'fetch', '--locked'], cwd=rust_copy, check=True)
    rust_offline = subprocess.run(
        ['cargo', 'build', '--release', '--locked', '--offline'], cwd=rust_copy,
        env={**os.environ, 'CARGO_TARGET_DIR': str(work_dir / 'rust-target-probe')},
        capture_output=True).returncode == 0
    rust_cmd = ['cargo', 'build', '--release', '--locked'] + (['--offline'] if rust_offline else [])
    if not rust_offline:
        print('offline cargo build failed; falling back to online (downloads not separated)', flush=True)

    rust_clean = []
    for i in range(clean_count):
        env = {'CARGO_TARGET_DIR': str(work_dir / f'rust-target-clean-{i}')}
        rust_clean.append(timed(rust_cmd, cwd=rust_copy, env=env))
    builds['rust_clean_samples_s'] = rust_clean
    builds['rust_clean_median_s'] = statistics.median(rust_clean)
    builds['rust_offline'] = rust_offline

    rust_file = rust_copy / 'src/engine.rs'
    rust_anchor = b'"Recovering"'
    rust_inc_env = {'CARGO_TARGET_DIR': str(work_dir / 'rust-target-inc')}
    # Populate the incremental target dir once so only the edited crate rebuilds.
    timed(rust_cmd, cwd=rust_copy, env=rust_inc_env)
    rust_original = rust_file.read_bytes()
    samples = []
    try:
        for i in range(incremental_count + 1):
            with_literal_probe(rust_file, rust_anchor, i)
            seconds = timed(rust_cmd, cwd=rust_copy, env=rust_inc_env)
            rust_file.write_bytes(rust_original)
            if i > 0:  # first build is the discarded warmup
                samples.append(seconds)
    finally:
        rust_file.write_bytes(rust_original)
    builds['rust_incremental_samples_s'] = samples
    builds['rust_incremental_median_s'] = statistics.median(samples)

    go_out = work_dir / 'octomus-agent-go-measured'
    go_cmd = ['go', 'build', '-trimpath', '-ldflags=-s', '-w', '-o', str(go_out), './cmd/octomus-agent']
    go_clean = []
    for i in range(clean_count):
        go_env = {'CGO_ENABLED': '0', 'GOCACHE': str(work_dir / f'gocache-clean-{i}')}
        go_clean.append(timed(go_cmd, cwd=go_src, env=go_env))
    builds['go_clean_samples_s'] = go_clean
    builds['go_clean_median_s'] = statistics.median(go_clean)

    go_file = go_src / 'internal/engine/engine.go'
    go_anchor = b'"Run once was interrupted before its planning transaction committed"'
    go_env = {'CGO_ENABLED': '0', 'GOCACHE': str(work_dir / 'gocache-inc')}
    timed(go_cmd, cwd=go_src, env=go_env)  # populate the incremental cache
    go_original = go_file.read_bytes()
    samples = []
    try:
        for i in range(incremental_count + 1):
            with_literal_probe(go_file, go_anchor, i)
            seconds = timed(go_cmd, cwd=go_src, env=go_env)
            go_file.write_bytes(go_original)
            if i > 0:  # first build is the discarded warmup
                samples.append(seconds)
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
        f"- Rust build: `cargo build --release --locked` in a pristine source copy after `cargo fetch --locked` (dependency download separated from compilation; offline={builds and builds.get('rust_offline')}); {args.clean_builds} clean builds, each with a fresh CARGO_TARGET_DIR; incremental = one function-body literal change in `src/engine.rs`, warm target dir, 1 discarded warmup + {args.incremental_builds} timed",
        f"- Go build: `CGO_ENABLED=0 go build -trimpath -ldflags=-s -w -o <out> ./cmd/octomus-agent`; {args.clean_builds} clean builds, each with a fresh GOCACHE (shared module cache); incremental = one function-body literal change in `internal/engine/engine.go`, warm cache, 1 discarded warmup + {args.incremental_builds} timed",
        f"- Frontend: identical prebuilt dashboard served via `--assets {args.assets}`",
        f"- Fixture: schema initialized by each binary on a fresh data dir, then `INSERT INTO records VALUES ('task', id, json)` in one transaction; published task, ~4 KiB `proposal.prompt`, timestamps inside the 14-day retention window; each run copies the seeded fixture",
        f"- Service: `--data-dir <isolated> --listen 127.0.0.1:<free port>`, `OCTOMUS_TOKEN` fixture token, paused before measuring (verified via control.paused)",
        f"- Latency: {args.runs} fresh service runs per impl per fixture; each run: {args.settle}s idle then VmRSS sample, {args.warmup} warmup + {args.requests} timed sequential `GET /api/state` requests on one keep-alive HTTP/1.1 connection, client-side wall clock (`time.monotonic`); p50/p95 are nearest-rank over pooled samples",
        f"- Idle RSS: `VmRSS` from `/proc/<pid>/status` after {args.settle}s idle, per run; median reported",
        '',
        '## Build times',
        '',
        '| Metric | Rust | Go |',
        '| --- | ---: | ---: |',
    ]
    if builds:
        rust_cl = ', '.join(f'{s:.1f}' for s in builds['rust_clean_samples_s'])
        go_cl = ', '.join(f'{s:.1f}' for s in builds['go_clean_samples_s'])
        rust_inc = ', '.join(f'{s:.2f}' for s in builds['rust_incremental_samples_s'])
        go_inc = ', '.join(f'{s:.2f}' for s in builds['go_incremental_samples_s'])
        lines += [
            f"| Clean median (s) | {builds['rust_clean_median_s']:.1f} | {builds['go_clean_median_s']:.1f} |",
            f"| Clean samples (s) | {rust_cl} | {go_cl} |",
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
        '| Tasks | Impl | Runs | p50 ms | p95 ms | Per-run p95 ms | max ms | Response B | Idle RSS MiB (median) | Seed s | DB MiB |',
        '| ---: | --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | ---: | ---: |',
    ]
    for scale in args.scales:
        for name in ['rust', 'go']:
            if scale not in runs.get(name, {}):
                continue
            r = runs[name][scale]
            per_run = ', '.join(fmt_ms(v) for v in r['per_run_p95_ms'])
            lines.append(
                f"| {scale} | {name} | {r['runs']} | {fmt_ms(r['p50_ms'])} | {fmt_ms(r['p95_ms'])} | {per_run} | {fmt_ms(r['max_ms'])} "
                f"| {r['response_bytes']} | {fmt_mib(r['idle_rss_kib_median'])} "
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
    parser.add_argument('--requests', type=int, default=TIMED_REQUESTS)
    parser.add_argument('--warmup', type=int, default=WARMUP_REQUESTS)
    parser.add_argument('--runs', type=int, default=SERVICE_RUNS, help='Fresh service runs per impl per fixture')
    parser.add_argument('--settle', type=float, default=IDLE_SETTLE_S, help='Idle seconds before the RSS sample and warmup in each run')
    parser.add_argument('--clean-builds', type=int, default=CLEAN_BUILDS)
    parser.add_argument('--incremental-builds', type=int, default=INCREMENTAL_BUILDS, help='Timed incremental samples after one discarded warmup')
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
        builds = measure_builds(Path(args.rust_src), Path(args.go_src), work_dir, args.clean_builds, args.incremental_builds)
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
                result = measure_state(name, binary, args.assets, scale, templates[name], work_dir,
                                       args.requests, args.warmup, args.runs, args.settle)
                runs[name][scale] = result
                print(f"  p50={result['p50_ms']:.2f}ms p95={result['p95_ms']:.2f}ms max={result['max_ms']:.2f}ms "
                      f"per-run-p95={['%.2f' % v for v in result['per_run_p95_ms']]} "
                      f"bytes={result['response_bytes']} rss={fmt_mib(result['idle_rss_kib_median'])}MiB "
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
