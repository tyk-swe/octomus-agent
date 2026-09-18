<script lang="ts">
  import bundle from 'virtual:public-run';
  import EvidenceFact from '../src/lib/EvidenceFact.svelte';
  import EvidenceText from '../src/lib/EvidenceText.svelte';
  import {
    planningVerdict,
    decisionCounts,
    decisionTone,
    reviewerSlot,
    reviewerLabel,
    verdictBadge,
    reviewerAgreement,
    outcomeVerdict,
    reviewVerdict,
    checksVerdict,
    commandBadge,
    commandExplanation,
    prVerdict,
    revisionMatchLabel
  } from '../src/lib/evidence';
  import { routeLabel } from '../src/lib/routes';
  import { publicPrUrl } from './links';
  const run = bundle.payload.evidence;
  let fragment = $state(window.location.hash);
  const selection = $derived(new URLSearchParams(fragment.slice(1)));
  function index(value: string | null): number {
    return value !== null && /^(0|[1-9][0-9]*)$/.test(value) ? Number(value) : -1;
  }
  const proposalIndex = $derived(fragment === '' ? 0 : index(selection.get('proposal')));
  const proposal = $derived(run.proposals[proposalIndex] ?? null);
  const taskIndex = $derived(
    selection.has('task')
      ? index(selection.get('task'))
      : proposal?.linked_tasks.length === 1
        ? 0
        : -1
  );
  const task = $derived(proposal?.linked_tasks[taskIndex] ?? null);
  const planning = planningVerdict(run.cycle);
  function jumpTo(section: string) {
    document.getElementById(`walkthrough-${section}`)?.focus();
  }
</script>

<svelte:window onhashchange={() => (fragment = window.location.hash)} />

{#snippet gaps(label: string, items: string[])}
  {#if items.length}<aside class="gaps">
      <h3>{label}</h3>
      <ul>
        {#each items as item}<li>{item}</li>{/each}
      </ul>
    </aside>{/if}
{/snippet}
{#snippet fact(label: string, value: string | number | boolean | null)}
  <div>
    <dt>{label}</dt>
    <dd>{value === null ? 'None recorded' : String(value)}</dd>
  </div>
{/snippet}

<a
  class="skip"
  href="#content"
  onclick={(event) => {
    event.preventDefault();
    document.getElementById('content')?.focus();
  }}>Skip to evidence</a
>
<header>
  <a class="brand" href="#proposal=0">OCTOMUS <span> / Explore a run</span></a>
  <span
    >{bundle.payload.mode === 'fixture'
      ? 'A guided sample · no account needed'
      : 'Public · static · recorded evidence'}</span
  >
</header>
<main id="content" tabindex="-1">
  <div class="provenance" data-testid="provenance">
    <strong
      >{bundle.payload.mode === 'fixture'
        ? 'Synthetic example — not a real run.'
        : 'Owner-reviewed public payload — recorded evidence.'}</strong
    >
    <p>
      {bundle.payload.mode === 'fixture'
        ? 'Every proposal, review and result below was made for this example. It is not live validation.'
        : 'This build carries the supplied approval reference for these exact public payload bytes. It makes no live observations.'}
    </p>
  </div>
  <section class="intro">
    <div>
      <span class="eyebrow">FROM PROPOSAL TO RECORDED RESULT</span>
      <h1>{bundle.payload.mode === 'fixture' ? 'Good ideas earn their PR.' : 'Explore a run.'}</h1>
      <p class="lead">
        Choose an idea. Read both reviews. Follow the result, including the work that didn’t ship.
      </p>
    </div>
    <dl>
      <EvidenceFact label={`${run.cycle.mode} cycle #${run.cycle.number}`} verdict={planning} />
    </dl>
  </section>
  <section class="panel" aria-label="Run record">
    <h2>The run at a glance</h2>
    <p class="muted">
      Planning records a decision for every proposal. Accepted work still needs code review and
      passing checks before a PR can be delivered.
    </p>
    <div class="badges">
      {#each decisionCounts(run.cycle.planning.decisions) as count}<span
          class={'badge ' + count.tone}>{count.count} {count.decision}</span
        >{/each}
    </div>
    <details class="run-metadata">
      <summary>Run metadata and grounding revision</summary>
      <dl class="facts">
        {@render fact('Repository', run.cycle.repository)}{@render fact(
          'Cycle identity',
          run.cycle.id
        )}
        {@render fact('Saved planning status', run.cycle.status)}{@render fact(
          'Planning finished',
          run.cycle.planning.planning_finished
        )}
        {@render fact('Started', run.cycle.started_at)}{@render fact(
          'Planning ended',
          run.cycle.completed_at
        )}
        {@render fact('Grounding revision', run.cycle.grounding_revision)}{@render fact(
          'Execution-enabled run',
          run.cycle.planning.creates_execution_queue
        )}
        {@render fact('Planning error recorded', run.cycle.planning.error_recorded)}{@render fact(
          'Reviewer batches saved',
          run.cycle.planning.reviewer_batches_saved
        )}
      </dl>
    </details>
    {#if run.cycle.mode === 'audit'}<p>
        Audit acceptance is a recommendation. Audit cycles do not create an execution queue.
      </p>{/if}
    {@render gaps('Recorded run gaps', run.gaps)}
  </section>
  <div class="explorer">
    <nav aria-label="Proposals">
      <h2>Proposals <span>{run.proposals.length}</span></h2>
      {#each run.proposals as p, i}
        <a href={`#proposal=${i}`} aria-current={proposalIndex === i ? 'true' : undefined}>
          <span class="position">{String(i + 1).padStart(2, '0')}</span><strong>{p.title}</strong
          ><span class={'badge ' + decisionTone(p.final_decision)}>{p.final_decision}</span>
          <small>{p.linked_tasks.length} linked task matches</small>
        </a>
      {:else}<p>No proposals recorded.</p>{/each}
    </nav>
    <div class="detail">
      {#if proposal}
        <div class="walkthrough" role="group" aria-label="Follow this proposal">
          <button onclick={() => jumpTo('idea')}>01 · The idea</button>
          <button onclick={() => jumpTo('review')}>02 · Both reviewers</button>
          <button onclick={() => jumpTo('decision')}>03 · The decision</button>
          <button onclick={() => jumpTo('result')}>04 · The result</button>
        </div>
        <section class="panel">
          <span class="eyebrow">PROPOSAL RECORD {proposalIndex + 1}</span>
          <h2 id="walkthrough-idea" tabindex="-1">{proposal.title}</h2>
          <dl class="facts">
            {@render fact('Identity', proposal.id)}{@render fact(
              'Target',
              proposal.target
            )}{@render fact('Tier', proposal.tier)}{@render fact('Category', proposal.category)}
          </dl>
          <EvidenceText label="Problem" value={proposal.problem} /><EvidenceText
            label="Benefit"
            value={proposal.benefit}
          /><EvidenceText label="Scope" value={proposal.scope} />
          <h3>Proposal evidence</h3>
          <ul>
            {#each proposal.evidence as item}<li>{item}</li>{:else}<li>None recorded.</li>{/each}
          </ul>
        </section>
        <section class="panel">
          <h2 id="walkthrough-review" tabindex="-1">Both reviewer slots</h2>
          <p>Slots are fixed and positional; missing or malformed evidence is never reassigned.</p>
          <div class="reviewers">
            {#each proposal.reviewer_verdicts as v}
              {@const badge = verdictBadge(v)}
              <article>
                <h3>{reviewerSlot(v.reviewer)} <small>{reviewerLabel(v.reviewer)}</small></h3>
                <span class={'badge ' + badge.tone}>{badge.label}</span>
                <p>Saved state: {v.state}</p>
                <EvidenceText label="Reviewer reason" value={v.reason ?? ''} />
                {#if v.note}<p class="note">{v.note}</p>{/if}
                {#if v.state === 'duplicate'}<p>
                    Several entries remain duplicated; no single assessment or reason was selected.
                  </p>{/if}
              </article>
            {/each}
          </div>
        </section>
        <section class="panel">
          <h2 id="walkthrough-decision" tabindex="-1">Final decision</h2>
          <span class={'badge ' + decisionTone(proposal.final_decision)}
            >{proposal.final_decision}</span
          >
          <dl>
            <EvidenceFact
              label="Reviewer agreement"
              verdict={reviewerAgreement(proposal.reviewer_verdicts)}
            />
          </dl>
          <EvidenceText label="Final rationale" value={proposal.final_reason} />
          <p>Deferred is not rejected. Acceptance is not execution.</p>
          {@render gaps('Recorded proposal gaps', proposal.gaps)}
        </section>
        <section class="panel">
          <h2 id="walkthrough-result" tabindex="-1">Linked tasks</h2>
          {#if proposal.linked_tasks.length === 0}<p>
              No linked task recorded.{run.cycle.mode === 'audit'
                ? ' An accepted audit proposal has no linked task by design.'
                : ''}
            </p>
          {:else}
            {#if proposal.linked_tasks.length > 1}<p>
                {proposal.linked_tasks.length} tasks match (cycle, proposal). Every match is preserved;
                none is selected for you.
              </p>{/if}
            <div class="task-links">
              {#each proposal.linked_tasks as t, i}<a
                  href={`#proposal=${proposalIndex}&task=${i}`}
                  aria-current={taskIndex === i ? 'true' : undefined}
                  >{t.id} · record {i + 1} · {t.status}</a
                >{/each}
            </div>
          {/if}
          {#if selection.has('task') && !task}<p role="status">
              The selected task record is unavailable. No other match was substituted.
            </p>
          {:else if proposal.linked_tasks.length > 1 && !task}<p>
              Select a task match to inspect its recorded evidence.
            </p>{/if}
        </section>
        {#if task}
          <section class="panel" aria-label="Selected task">
            <h2>Task record: {task.id}</h2>
            <dl class="facts">
              <EvidenceFact label="Recorded outcome" verdict={outcomeVerdict(task)} />
              {@render fact('Branch', task.branch)}{@render fact(
                'Retries',
                task.attempts
              )}{@render fact('Blocked reason', task.blocked_reason)}{@render fact(
                'Error recorded',
                task.error_recorded
              )}
              {@render fact('Created', task.created_at)}{@render fact('Updated', task.updated_at)}
              {@render fact('Source revision', task.revisions.source)}{@render fact(
                'Comparison base',
                task.revisions.comparison_base
              )}
              {@render fact('Default branch', task.revisions.default_branch)}{@render fact(
                'Output revision',
                task.revisions.output
              )}
            </dl>
            {@render gaps('Recorded task gaps', task.gaps)}
            <h3>Requested session routes</h3>
            <p>
              Requested routes are distinct from independently reported model identity. Runtime
              model identity is not independently reported here.
            </p>
            {#each task.sessions as session}<article class="record">
                <h4>{session.role}</h4>
                <p>{routeLabel(session.requested_route)}</p>
                <p>Session {session.id} · saved status {session.status} · {session.started_at}</p>
              </article>{:else}<p>No sessions recorded.</p>{/each}
          </section>
          <section class="panel">
            <h2>Latest recorded code review</h2>
            <dl><EvidenceFact label="Latest review standing" verdict={reviewVerdict(task)} /></dl>
            <p>
              {task.latest_review.rounds_recorded} rounds recorded. Only the latest saved round is available
              here; this is not the full review history.
            </p>
            {#if task.latest_review.latest}
              {@const review = task.latest_review.latest}
              <dl class="facts">
                {@render fact('Completed', review.completed)}{@render fact(
                  'Summary present',
                  review.summary_present
                )}{@render fact('Review session', review.session_id)}{@render fact(
                  'Recorded',
                  review.created_at
                )}{@render fact('Review comparison base', review.comparison_base)}{@render fact(
                  'Reviewed revision',
                  review.revision
                )}
              </dl>
              <p>{revisionMatchLabel(review.matches_output_revision).label}</p>
              <p>The summary text is not exported. Structured findings:</p>
              {#each review.findings as finding}<article class="finding">
                  <h3>{finding.priority} · {finding.title}</h3>
                  <code>{finding.file}</code><EvidenceText
                    label="Finding detail"
                    value={finding.detail}
                  />
                </article>{:else}<p>No findings in the latest saved round.</p>{/each}
            {:else}<p>No review round recorded. Missing evidence is not a pass.</p>{/if}
          </section>
          <section class="panel">
            <h2>Configured checks</h2>
            <dl><EvidenceFact label="Configured check results" verdict={checksVerdict(task)} /></dl>
            {#each task.required_commands.commands as command}
              {@const badge = commandBadge(command.state)}
              <article class="record">
                <h3><code>{command.command}</code></h3>
                <span class={'badge ' + badge.tone}>{badge.label}</span>
                <p>{commandExplanation(command, task.revisions.output)}</p>
                <dl class="facts">
                  {@render fact('Recorded result count', command.results_recorded)}{@render fact(
                    'Latest success',
                    command.latest_success
                  )}{@render fact('Latest revision', command.latest_revision)}{@render fact(
                    'Latest recorded at',
                    command.latest_created_at
                  )}
                </dl>
              </article>
            {/each}
            <p>
              The latest result governs; a newer failure invalidates an older pass. Command output
              is not exported.
            </p>
          </section>
          <section class="panel">
            <h2>Recorded pull request</h2>
            <dl><EvidenceFact label="Saved reference" verdict={prVerdict(task)} /></dl>
            {#if task.pull_request?.url}
              {@const url = publicPrUrl(task.pull_request.url)}
              {#if url}<a class="pr-link" href={url} target="_blank" rel="noopener noreferrer"
                  >Open recorded PR {task.pull_request.number === null
                    ? ''
                    : `#${task.pull_request.number}`}</a
                >
              {:else}<p>
                  URL not linked by the public link policy: <code>{task.pull_request.url}</code>
                </p>{/if}
            {/if}
            <p>
              A recorded PR describes delivery, not current GitHub state or merge. Published is not
              merged.
            </p>
          </section>
        {/if}
      {:else if run.proposals.length}<section class="panel">
          <h2>Proposal selection unavailable</h2>
          <p>
            No recorded proposal matches this fragment. Choose a proposal; nothing was substituted.
          </p>
        </section>{/if}
    </div>
  </div>
  <section class="panel">
    <h2>Limitations and provenance</h2>
    <p>
      No full review history, command-output transcript or complete replay timeline is provided by
      this schema.
    </p>
    <ul>
      {#each run.limitations as limitation}<li>{limitation}</li>{/each}
    </ul>
    <h3>Original operator-export warning (unchanged)</h3>
    <p>{run.review_requirement}</p>
    <p>
      Assembled: {run.generated_at} · Evidence schema {run.schema_version} · Public wrapper schema 1
    </p>
    <dl>
      {@render fact('Public payload SHA-256', bundle.hash)}{#if bundle.approval}{@render fact(
          'Supplied owner approval reference',
          bundle.approval.approval_reference
        )}{/if}
    </dl>
    <p>A hash binds reviewed bytes, not proof of truth or an independent signature.</p>
    <a href="./public-run.json" download>Download exact public payload</a>
  </section>
  <section class="panel install" aria-labelledby="install-heading">
    <h2 id="install-heading">Run it on your own repository</h2>
    <p>
      Octomus is built from source today; public release binaries and the crates.io package are
      pending. You need a dedicated Ubuntu 24.04 VM, your own Codex or OpenCode provider login and a
      dedicated GitHub identity restricted to one repository. The README's first-run path walks
      through entering, saving and checking the configuration before you explicitly choose an audit
      or a single run.
    </p>
    <a
      class="install-link"
      href="https://github.com/tyk-swe/octomus-agent#getting-started"
      target="_blank"
      rel="noopener noreferrer">Read the first-run guide on GitHub</a
    >
  </section>
</main>
<footer>Octomus · Recorded evidence, with its gaps intact.</footer>
