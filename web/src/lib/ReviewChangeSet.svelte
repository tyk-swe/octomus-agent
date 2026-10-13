<script lang="ts">
  import RelativeTime from './RelativeTime.svelte';
  import Sha from './Sha.svelte';
  let {
    comparisonBase,
    revision,
    createdAt,
    oncopy
  }: {
    comparisonBase: string;
    revision: string;
    createdAt: string;
    oncopy: (value: string, label: string) => void;
  } = $props();
</script>

<dl class="fact-row">
  <div>
    <dt>Reviewed change set</dt>
    <dd>
      <Sha value={comparisonBase} label="Comparison base" {oncopy} />
      <span aria-hidden="true">→</span>
      <Sha value={revision} label="Reviewed revision" {oncopy} />
    </dd>
  </div>
  <div>
    <dt>Recorded</dt>
    <dd><RelativeTime value={createdAt} /></dd>
  </div>
</dl>
