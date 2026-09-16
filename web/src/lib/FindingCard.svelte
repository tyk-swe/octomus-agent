<script lang="ts">
  import EvidenceText from './EvidenceText.svelte';
  import type { FindingEvidence } from './types';
  let { finding }: { finding: FindingEvidence } = $props();
</script>

<div class="finding">
  <span class="tier">{finding.priority}</span>
  <h4>{finding.title}</h4>
  <code>{finding.file}</code>
  <EvidenceText label="Finding detail" value={finding.detail} />
</div>
