import { defineStore } from "pinia";

// Live monitor status pushed over SSE from GET /api/v1/events.
export interface LiveStatus {
  status: string;
  latency_ms: number;
  ts: string;
}

// The EventSource is a process singleton kept outside reactive state.
let es: EventSource | null = null;
// Watchdog against silent socket death (mobile Wi-Fi → LTE switches kill the
// TCP connection without firing onerror): the server pings every 25s as a real
// SSE event, so >75s of total silence means the stream is dead.
let watchdog: ReturnType<typeof setInterval> | null = null;
let lastSeen = 0;
const SILENCE_LIMIT_MS = 75_000;

export const useLive = defineStore("live", {
  state: () => ({
    statuses: {} as Record<string, LiveStatus>,
    connected: false,
    // started distinguishes "stream was requested and dropped" (show the
    // reconnecting chip) from "no view has asked for live updates yet".
    started: false,
  }),
  actions: {
    connect() {
      if (es) return; // already streaming
      this.started = true;
      lastSeen = Date.now();
      es = new EventSource("/api/v1/events");
      es.addEventListener("status", (e) => {
        lastSeen = Date.now();
        try {
          const d = JSON.parse((e as MessageEvent).data) as { monitor_id?: string } & LiveStatus;
          if (d.monitor_id) {
            this.statuses[d.monitor_id] = { status: d.status, latency_ms: d.latency_ms, ts: d.ts };
          }
        } catch {
          /* ignore malformed frames */
        }
      });
      // The server's keepalive: proves the socket is alive on a quiet system.
      es.addEventListener("ping", () => {
        lastSeen = Date.now();
        this.connected = true;
      });
      es.onopen = () => {
        lastSeen = Date.now();
        this.connected = true;
      };
      es.onerror = () => {
        // EventSource auto-reconnects; just reflect the transient drop.
        this.connected = false;
      };
      // Silent-death watchdog: onerror never fires when the TCP connection dies
      // under the browser (network switch), so recreate on prolonged silence.
      // `started` stays true → the header's reconnecting chip shows meanwhile.
      if (watchdog === null) {
        watchdog = setInterval(() => {
          if (!es || Date.now() - lastSeen < SILENCE_LIMIT_MS) return;
          this.connected = false;
          es.close();
          es = null;
          this.connect();
        }, 10_000);
      }
    },
    disconnect() {
      if (watchdog !== null) {
        clearInterval(watchdog);
        watchdog = null;
      }
      es?.close();
      es = null;
      this.connected = false;
      this.started = false;
    },
  },
});
