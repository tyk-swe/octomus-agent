#!/usr/bin/env python3
import sys
from harness import run_selected, select_scenarios
import e2e
import e2e_baseline
import e2e_hardening
import e2e_notifications
import e2e_runners

SUITES = [
    ('e2e', e2e.SCENARIOS),
    ('baseline', e2e_baseline.SCENARIOS),
    ('notifications', e2e_notifications.SCENARIOS),
    ('runners', e2e_runners.SCENARIOS),
    ('hardening', e2e_hardening.SCENARIOS),
]

if __name__ == '__main__':
    run_selected('integration', select_scenarios(SUITES, sys.argv[1:]), [])
