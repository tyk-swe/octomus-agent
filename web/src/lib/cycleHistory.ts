import { get } from './api';
import type { CycleSummary, Page } from './types';

type ReadPage = (path: string, signal: AbortSignal) => Promise<Page<CycleSummary>>;
type Segment = { rows: CycleSummary[]; cursor: number | null };
export type CycleHistoryState = {
  rows: CycleSummary[];
  cursor: number | null;
  loading: boolean;
  error: string;
};

// Polling refreshes one newest page and, when necessary, the selected older cycle.
// Older pages stay cached. A gap after a long absence is filled by Load older before
// continuing from the opaque cursor at the end of the retained history.
export class CycleHistory {
  private state: CycleHistoryState = { rows: [], cursor: null, loading: false, error: '' };
  private segments: Segment[] = [];
  // An exact lookup proves nothing about the unloaded pages around that row.
  private lookups = new Map<string, CycleSummary>();
  private flight: { controller: AbortController; promise: Promise<boolean> } | null = null;

  constructor(
    private changed: (state: CycleHistoryState) => void,
    private read: ReadPage = get
  ) {}

  cancel() {
    this.flight?.controller.abort();
    this.flight = null;
    this.publish({ loading: false });
  }

  refresh(selected = 'all') {
    return this.load(false, selected);
  }

  older() {
    return this.load(true, 'all');
  }

  private publish(patch: Partial<CycleHistoryState>) {
    this.state = { ...this.state, ...patch };
    this.changed(this.state);
  }

  private load(more: boolean, selected: string): Promise<boolean> {
    if (this.flight) return this.flight.promise;
    if (more && this.state.cursor === null) return Promise.resolve(false);
    const controller = new AbortController();
    const signal = AbortSignal.any([controller.signal, AbortSignal.timeout(90000)]);
    const source = more ? this.segments.find((segment) => segment.cursor !== null) : undefined;
    const before = source?.cursor ?? null;
    this.publish({ loading: true });
    const flight = { controller, promise: Promise.resolve(false) };
    this.flight = flight;
    flight.promise = (async () => {
      try {
        const params = new URLSearchParams({ limit: '100' });
        if (before !== null) params.set('before', String(before));
        const page = await this.read(`/cycles?${params}`, signal);
        let selectedPage: Page<CycleSummary> | undefined;
        if (!more && selected !== 'all' && !page.items.some((row) => row.id === selected)) {
          selectedPage = await this.read(
            `/cycles?${new URLSearchParams({ limit: '1', cycle: selected })}`,
            signal
          );
        }
        signal.throwIfAborted();
        if (this.flight !== flight) return false;
        this.merge(page, source);
        if (selectedPage) {
          const selectedRow = selectedPage.items.find((row) => row.id === selected);
          let cached = false;
          for (const segment of this.segments) {
            if (segment.rows.some((row) => row.id === selected)) {
              cached = true;
              segment.rows = mergeRows(
                segment.rows.filter((row) => row.id !== selected),
                selectedRow ? [selectedRow] : []
              );
            }
          }
          if (!cached && selectedRow) this.lookups.set(selected, selectedRow);
          else this.lookups.delete(selected);
          this.publishRows();
        }
        this.publish({ error: '' });
        return true;
      } catch (error) {
        if (controller.signal.aborted || this.flight !== flight) return false;
        this.publish({ error: (error as Error).message });
        return false;
      } finally {
        if (this.flight === flight) {
          this.flight = null;
          this.publish({ loading: false });
        }
      }
    })();
    return flight.promise;
  }

  private merge(page: Page<CycleSummary>, source?: Segment) {
    for (const row of page.items) this.lookups.delete(row.id);
    if (!source && page.next_cursor === null) {
      this.lookups.clear();
      this.segments = [{ rows: page.items, cursor: null }];
    } else {
      const updated = new Set(page.items.map((row) => row.id));
      const joins = this.segments.filter(
        (segment) => segment === source || segment.rows.some((row) => updated.has(row.id))
      );
      const rows = mergeRows(
        joins.flatMap((segment) => segment.rows),
        page.items
      );
      const tail = rows.at(-1)?.id;
      const cursor =
        !page.items.length || page.items.some((row) => row.id === tail)
          ? page.next_cursor
          : joins.find((segment) => segment.rows.at(-1)?.id === tail)!.cursor;
      this.segments = [
        ...this.segments.filter((segment) => !joins.includes(segment)),
        { rows, cursor }
      ].sort((a, b) => (b.rows[0]?.number ?? 0) - (a.rows[0]?.number ?? 0));
    }
    this.publishRows();
  }

  private publishRows() {
    this.publish({
      rows: mergeRows(
        this.segments.flatMap((segment) => segment.rows),
        [...this.lookups.values()]
      ),
      cursor: this.segments.find((segment) => segment.cursor !== null)?.cursor ?? null
    });
  }
}

function mergeRows(previous: CycleSummary[], updated: CycleSummary[]) {
  const rows = new Map(previous.map((row) => [row.id, row]));
  for (const row of updated) rows.set(row.id, row);
  return [...rows.values()].sort((a, b) => b.number - a.number);
}
