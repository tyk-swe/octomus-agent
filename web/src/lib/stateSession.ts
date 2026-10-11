import type { Snapshot } from './types';

// The live state session owns polling and confirming reads. An accepted mutation
// invalidates reads begun before it; acceptance stays pending until a newer read
// succeeds, even when that read fails or a previous poll finishes late.
export class StateSession {
  private generation = 0;
  private controller = new AbortController();
  private flight: Promise<void> | null = null;
  private queued = false;

  constructor(
    private read: (signal: AbortSignal) => Promise<Snapshot>,
    private accepted: (snapshot: Snapshot) => void,
    private failed: (error: Error) => void,
    private pending: (pending: boolean) => void
  ) {}

  get active() {
    return this.flight !== null;
  }

  invalidate() {
    this.generation++;
    this.pending(true);
    if (this.flight) this.queued = true;
  }

  reset() {
    this.generation++;
    this.controller.abort();
    this.controller = new AbortController();
    this.flight = null;
    this.queued = false;
    this.pending(false);
  }

  refresh(): Promise<void> {
    if (this.flight) {
      this.queued = true;
      return this.flight;
    }
    const controller = this.controller;
    const flight = (async () => {
      do {
        this.queued = false;
        const generation = this.generation;
        try {
          const value = await this.read(controller.signal);
          if (controller !== this.controller) return;
          if (generation !== this.generation) {
            this.queued = true;
            continue;
          }
          this.accepted(value);
          this.pending(false);
        } catch (error) {
          if (controller !== this.controller) return;
          if (generation !== this.generation) {
            this.queued = true;
            continue;
          }
          this.failed(error as Error);
        }
      } while (this.queued && !controller.signal.aborted);
    })();
    this.flight = flight;
    void flight.finally(() => {
      if (this.flight === flight) this.flight = null;
    });
    return flight;
  }
}
