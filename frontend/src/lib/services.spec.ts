import { describe, expect, it } from "vitest";

import { humanDuration, lagExact, lagLabel, sealedLabel } from "./services";

describe("sealedLabel", () => {
  it("renders the watermark in UTC without sub-second noise", () => {
    expect(sealedLabel("2026-08-16T09:38:00.123456Z")).toBe("2026-08-16 09:38:00Z");
  });

  // A malformed timestamp is shown as-is rather than as "Invalid Date": the operator can then
  // see WHAT the server sent, which is the only useful thing at that point.
  it("passes an unparseable value through", () => {
    expect(sealedLabel("not-a-date")).toBe("not-a-date");
  });
});

describe("lagLabel", () => {
  const now = new Date("2026-08-16T09:41:12Z");

  // A healthy service is normally a couple of minutes behind — bucket 60s plus the
  // late-arrival grace. Calling that out on every row would train the operator to ignore the
  // one place the lag actually matters.
  it("says nothing while the service seals at its normal cadence", () => {
    expect(lagLabel("2026-08-16T09:38:00Z", now)).toBe("");
  });

  it("names the lag once a service has actually fallen behind", () => {
    expect(lagLabel("2026-08-16T07:15:00Z", now)).toBe("2h 26m");
  });

  // A watermark ahead of the clock (a slow client, a skewed laptop) must not render as a
  // negative or a nonsense lag.
  it("stays quiet when the watermark is ahead of the local clock", () => {
    expect(lagLabel("2026-08-16T09:45:00Z", now)).toBe("");
  });
});

describe("lagExact", () => {
  const now = new Date("2026-08-16T09:41:12Z");

  // On the detail screen the delta IS the subject, so the healthy lag is shown too.
  it("reports the healthy lag the list suppresses", () => {
    expect(lagExact("2026-08-16T09:38:00Z", now)).toBe("3m 12s");
  });

  it("floors a future watermark at zero rather than going negative", () => {
    expect(lagExact("2026-08-16T09:45:00Z", now)).toBe("0s");
  });
});

describe("humanDuration", () => {
  it("scales the unit to the magnitude", () => {
    expect(humanDuration(45_000)).toBe("45s");
    expect(humanDuration(90_000)).toBe("1m 30s");
    expect(humanDuration(3 * 3600_000 + 25 * 60_000)).toBe("3h 25m");
    expect(humanDuration(50 * 3600_000)).toBe("2d 2h");
  });
});
