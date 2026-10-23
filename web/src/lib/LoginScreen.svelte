<script lang="ts">
  import Icon from './Icon.svelte';

  let {
    accessToken = $bindable(''),
    error,
    busy,
    onsubmit
  }: { accessToken: string; error: string; busy: boolean; onsubmit: () => void } = $props();
</script>

<main class="login-page">
  <div class="login-brand">
    <img src="/favicon.svg" alt="" width="34" height="34" /><span
      >octomus<span class="brand-light">agent</span></span
    >
  </div>
  <section class="login-card">
    <h1>Connect to Octomus</h1>
    <form
      onsubmit={(e) => {
        e.preventDefault();
        onsubmit();
      }}
    >
      <label for="access-token">Operator access token</label><input
        id="access-token"
        type="password"
        bind:value={accessToken}
        placeholder="Enter your access token"
        autocomplete="off"
        required
      />{#if error}<div class="notice error" role="alert">{error}</div>{/if}<button
        class="button primary"
        disabled={busy || !accessToken.trim()}
        >{busy ? 'Connecting…' : 'Open dashboard'}<Icon name="arrow" size={18} /></button
      >
    </form>
    <div class="login-security">
      <Icon name="shield" size={16} /><span>Your token stays in this tab’s memory.</span>
    </div>
  </section>
</main>
