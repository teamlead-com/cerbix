import { defineStore } from "pinia";

// Live monitor status pushed over SSE from GET /api/v1/events.
export interface LiveStatus {
  status: string;
  latency_ms: number;
  ts: string;
}

// The EventSource is a process singleton kept outside reactive state.
let es: EventSource | null = null;

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
      es = new EventSource("/api/v1/events");
      es.addEventListener("status", (e) => {
        try {
          const d = JSON.parse((e as MessageEvent).data) as { monitor_id?: string } & LiveStatus;
          if (d.monitor_id) {
            this.statuses[d.monitor_id] = { status: d.status, latency_ms: d.latency_ms, ts: d.ts };
          }
        } catch {
          /* ignore malformed frames */
        }
      });
      es.onopen = () => {
        this.connected = true;
      };
      es.onerror = () => {
        // EventSource auto-reconnects; just reflect the transient drop.
        this.connected = false;
      };
    },
    disconnect() {
      es?.close();
      es = null;
      this.connected = false;
      this.started = false;
    },
  },
});
