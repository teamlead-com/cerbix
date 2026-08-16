// The ONE objective rule, client side — a mirror of Go's domain.CanonicalObjective
// (D-0165): SLA objectives live in the OPEN interval (0,100), canonical at four decimal
// places (maximum 99.9999). The raw value is judged first (100.00004 is rejected as typed,
// never rounded into range), then rounded half-up — Math.round is half-away-from-zero,
// which coincides with Go's math.Round on the positive values the raw bound admits — and
// the canonical result must stay inside (0,100) too: 99.99995 rounds to 100 and is
// rejected, 0.00001 rounds to zero and is rejected. Callers send/display the CANONICAL
// value, so what the user confirmed and what the server stores are the same number.
export function canonicalObjective(v: number): number | null {
  if (!Number.isFinite(v) || v <= 0 || v >= 100) return null;
  const canonical = Math.round(v * 10000) / 10000;
  if (canonical <= 0 || canonical >= 100) return null;
  return canonical;
}
