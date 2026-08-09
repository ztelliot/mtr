import { afterEach, describe, expect, it, vi } from "vitest";
import { StatusFallbackPoller } from "./statusFallback";

afterEach(() => {
  vi.useRealTimers();
});

describe("StatusFallbackPoller", () => {
  it("polls with bounded exponential backoff until the job is terminal", async () => {
    vi.useFakeTimers();
    const poll = vi.fn()
      .mockResolvedValueOnce(false)
      .mockResolvedValueOnce(false)
      .mockResolvedValueOnce(true);
    const fallback = new StatusFallbackPoller(4_000, 30_000);

    fallback.start("job-1", poll);
    await vi.advanceTimersByTimeAsync(4_000);
    expect(poll).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(7_999);
    expect(poll).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(poll).toHaveBeenCalledTimes(2);
    await vi.advanceTimersByTimeAsync(16_000);
    expect(poll).toHaveBeenCalledTimes(3);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(poll).toHaveBeenCalledTimes(3);
  });

  it("does not reschedule an in-flight poll after it is stopped", async () => {
    vi.useFakeTimers();
    let finishPoll: ((terminal: boolean) => void) | undefined;
    const poll = vi.fn(() => new Promise<boolean>((resolve) => {
      finishPoll = resolve;
    }));
    const fallback = new StatusFallbackPoller(1_000, 8_000);

    fallback.start("job-1", poll);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(poll).toHaveBeenCalledTimes(1);
    fallback.stopAll();
    finishPoll?.(false);
    await Promise.resolve();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(poll).toHaveBeenCalledTimes(1);
  });
});
