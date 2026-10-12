export type ResourceState<T> = {
  value: T | null;
  loading: boolean;
  stale: boolean;
  error: string;
};

// One owner for a resource's request identity, retained content and disposal.
// Coalescing lets slow evidence finish while remembering only the newest key, and a
// request for the loading key supersedes whatever queued behind it, so only the newest
// requested key can become the confirmed fresh state; retained content stays visible as
// stale until then. Forced reads invalidate any request started before an accepted mutation.
export class Resource<T, K = string> {
  private state: ResourceState<T> = { value: null, loading: false, stale: false, error: '' };
  private flight: { key: K; controller: AbortController; promise: Promise<boolean> } | null = null;
  private queued: { key: K } | null = null;
  private disposed = false;

  constructor(
    private read: (key: K, signal: AbortSignal) => Promise<T>,
    private changed: (state: ResourceState<T>) => void,
    private coalesce = false
  ) {}

  cancel() {
    this.flight?.controller.abort();
    this.flight = null;
    this.queued = null;
    this.publish({ loading: false });
  }

  clear() {
    this.cancel();
    this.publish({ value: null, stale: false, error: '' });
  }

  invalidate() {
    this.cancel();
    this.publish({ stale: this.state.value !== null });
  }

  dispose() {
    this.cancel();
    this.disposed = true;
  }

  refresh(key: K, force = false): Promise<boolean> {
    if (this.disposed) return Promise.resolve(false);
    if (this.flight && !force) {
      if (this.flight.key === key) {
        // This request is the newest one, so any key queued behind the loading read is
        // obsolete intent: letting it run afterwards would confirm a superseded key.
        this.queued = null;
        return this.flight.promise;
      }
      if (this.coalesce) {
        this.queued = { key };
        this.publish({ stale: this.state.value !== null });
        return this.flight.promise;
      }
    }
    this.cancel();
    const flight = { key, controller: new AbortController(), promise: Promise.resolve(false) };
    this.flight = flight;
    this.publish({ loading: true, stale: this.state.value !== null });
    flight.promise = (async () => {
      try {
        const value = await this.read(key, flight.controller.signal);
        if (this.flight !== flight || flight.controller.signal.aborted) return false;
        this.publish({ value, stale: this.queued !== null, error: '' });
        return true;
      } catch (error) {
        if (this.flight !== flight || flight.controller.signal.aborted) return false;
        this.publish({ error: (error as Error).message, stale: this.state.value !== null });
        return false;
      } finally {
        if (this.flight === flight) {
          this.flight = null;
          this.publish({ loading: false });
          const queued = this.queued;
          this.queued = null;
          if (queued) void this.refresh(queued.key);
        }
      }
    })();
    return flight.promise;
  }

  private publish(patch: Partial<ResourceState<T>>) {
    this.state = { ...this.state, ...patch };
    this.changed(this.state);
  }
}
