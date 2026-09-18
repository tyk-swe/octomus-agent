<script lang="ts">
  import Icon from './Icon.svelte';

  /**
   * The unauthenticated screen. It holds no service state of its own: the token
   * is bound back to the page, which clears it as soon as a connection succeeds
   * so it never outlives the request that used it.
   */
  let {
    accessToken = $bindable(''),
    error,
    busy,
    onsubmit
  }: { accessToken: string; error: string; busy: boolean; onsubmit: () => void } = $props();
</script>

<main class="login-page">
  <div class="login-brand">
    <img src="/favicon.svg" alt="" width="40" height="40" /><span
      >octomus<span class="brand-light">agent</span></span
    >
  </div>
  <div class="login-layout">
    <section class="login-story">
      <span class="eyebrow">A FEW EXTRA ARMS FOR YOUR REPOSITORY</span>
      <h1>Your next<br />improvement.<br /><em>Ready for review.</em></h1>
      <p>
        Useful ideas. Challenged proposals. Reviewed pull requests.<br />Your project, with a little
        help from Octomus.
      </p>
      <div class="login-pipeline">
        <span><Icon name="proposals" />Discover</span><i></i><span><Icon name="code" />Build</span
        ><i></i><span><Icon name="shield" />Review</span><i></i><span
          ><Icon name="prs" />Deliver</span
        >
      </div>
    </section>
    <section class="login-card">
      <span class="login-mark"><Icon name="key" size={24} /></span>
      <h2>Your project’s control room.</h2>
      <p>
        Connect to your private Octomus service to follow the work, inspect the evidence, and choose
        what happens next.
      </p>
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
        <Icon name="shield" size={16} /><span
          >Private operator access. Your token stays in this tab’s memory.</span
        >
      </div>
    </section>
  </div>
  <footer class="login-footer">
    <span>Self-hosted preview.</span><span>Octomus opens the PR. You decide what merges.</span>
  </footer>
</main>
