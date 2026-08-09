export type StatusFallbackPoll = () => Promise<boolean>;

type StatusFallbackEntry = {
  delayMS: number;
  poll: StatusFallbackPoll;
  timer: ReturnType<typeof setTimeout> | null;
};

export class StatusFallbackPoller {
  private readonly entries = new Map<string, StatusFallbackEntry>();

  constructor(
    private readonly initialDelayMS: number,
    private readonly maxDelayMS: number
  ) {
    if (initialDelayMS <= 0 || maxDelayMS < initialDelayMS) {
      throw new Error("invalid status fallback polling interval");
    }
  }

  start(key: string, poll: StatusFallbackPoll): void {
    this.stop(key);
    const entry: StatusFallbackEntry = {
      delayMS: this.initialDelayMS,
      poll,
      timer: null
    };
    this.entries.set(key, entry);
    this.schedule(key, entry);
  }

  stop(key: string): void {
    const entry = this.entries.get(key);
    if (!entry) {
      return;
    }
    this.entries.delete(key);
    if (entry.timer !== null) {
      clearTimeout(entry.timer);
      entry.timer = null;
    }
  }

  stopAll(): void {
    for (const key of this.entries.keys()) {
      this.stop(key);
    }
  }

  private schedule(key: string, entry: StatusFallbackEntry): void {
    entry.timer = setTimeout(() => {
      entry.timer = null;
      void this.run(key, entry);
    }, entry.delayMS);
  }

  private async run(key: string, entry: StatusFallbackEntry): Promise<void> {
    let terminal = false;
    try {
      terminal = await entry.poll();
    } catch {
      // A failed status request is transient for the fallback poller.
    }
    if (this.entries.get(key) !== entry) {
      return;
    }
    if (terminal) {
      this.entries.delete(key);
      return;
    }
    entry.delayMS = Math.min(entry.delayMS * 2, this.maxDelayMS);
    this.schedule(key, entry);
  }
}
