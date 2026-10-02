#!/usr/bin/env python3
"""Guard CI checkout provenance with a local branch/merge-ref movement fixture."""
from pathlib import Path
import re
import tempfile

from harness import git

PROJECT = Path(__file__).resolve().parents[1]
EXPECTED_REF = '${{ inputs.ref || github.sha }}'


def checkout_refs(source):
    """Read checkout steps in our workflow's block-style step/with layout."""
    lines = source.splitlines()
    found = []
    for index, line in enumerate(lines):
        match = re.fullmatch(r'( *)(- )?uses: actions/checkout@\S+', line)
        if not match:
            continue
        step_indent = len(match[1]) - (0 if match[2] else 2)
        block = []
        for following in lines[index + 1:]:
            if following.strip() and len(following) - len(following.lstrip()) <= step_indent:
                break
            block.append(following)
        assert ' ' * (step_indent + 2) + 'with:' in block, f'checkout at line {index + 1} lacks with'
        refs = [line.strip()[5:] for line in block if line.startswith(' ' * (step_indent + 4) + 'ref: ')]
        assert len(refs) == 1, f'checkout at line {index + 1} must have exactly one ref'
        found.append((index + 1, refs[0]))
    assert found, 'CI must check out source before testing it'
    assert len(found) == source.count('actions/checkout@'), 'unrecognized checkout layout; extend the guard'
    return found


def assert_checkout_contract(source):
    refs = checkout_refs(source)
    for line, value in refs:
        assert value == EXPECTED_REF, f'checkout at line {line}: {value!r}; want event SHA with explicit caller override'
    return refs


def selected_ref(expression, context):
    # Only this two-string fallback is allowed by the contract. An absent input
    # is empty, and GitHub's || selects the first nonempty string.
    assert expression == EXPECTED_REF
    return next(context.get(name.strip(), '') for name in expression[3:-3].split('||')
                if context.get(name.strip(), ''))


def moving_ref_fixture(expression):
    with tempfile.TemporaryDirectory(prefix='octomus-ci-ref-') as directory:
        root = Path(directory)
        git('init', '-b', 'main', cwd=root)
        git('config', 'user.name', 'Fixture', cwd=root)
        git('config', 'user.email', 'fixture@example.com', cwd=root)
        code = root / 'source.txt'
        code.write_text('event code\n')
        git('add', '.', cwd=root)
        git('commit', '-m', 'Event commit', cwd=root)
        event_sha = git('rev-parse', 'HEAD', cwd=root)
        code.write_text('later code\n')
        git('commit', '-am', 'Branch moved after the event', cwd=root)
        later_sha = git('rev-parse', 'HEAD', cwd=root)
        git('update-ref', 'refs/pull/7/merge', later_sha, cwd=root)
        git('tag', 'v0.0.0-fixture', later_sha, cwd=root)
        for label, event_ref, override, expected in [
            ('push', 'refs/heads/main', None, event_sha),
            ('pull request merge', 'refs/pull/7/merge', None, event_sha),
            ('reusable default', 'refs/heads/main', '', event_sha),
            ('explicit release tag', 'refs/heads/main', 'v0.0.0-fixture', later_sha),
            ('explicit commit', 'refs/heads/main', later_sha, later_sha),
        ]:
            assert git('rev-parse', event_ref, cwd=root) == later_sha
            context = {'github.sha': event_sha, 'github.ref': event_ref}
            if override is not None:
                context['inputs.ref'] = override
            git('checkout', '--detach', selected_ref(expression, context), cwd=root)
            assert git('rev-parse', 'HEAD', cwd=root) == expected, label
            assert code.read_text() == ('event code\n' if expected == event_sha else 'later code\n'), label
            print(f'PASS CI checkout provenance: {label}', flush=True)


def main():
    source = (PROJECT / '.github/workflows/ci.yml').read_text()
    refs = assert_checkout_contract(source)
    # A single job drifting or losing its ref must fail the guard.
    for invalid in [source.replace(EXPECTED_REF, '${{ inputs.ref || github.ref }}', 1),
                    source.replace('ref: ' + EXPECTED_REF, 'fetch-depth: 1', 1)]:
        try:
            assert_checkout_contract(invalid)
        except AssertionError:
            pass
        else:
            raise AssertionError('the checkout guard accepted an unpinned job')
    moving_ref_fixture(refs[0][1])
    print(f'PASS CI checkout contract: all {len(refs)} source checkouts pin the event SHA unless explicitly overridden')


if __name__ == '__main__':
    main()
