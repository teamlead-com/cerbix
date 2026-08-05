// Structured postmortem helpers. The backend stores a single markdown `body`;
// the UI presents it as fixed sections (Summary / Root cause / Resolution /
// Action items) serialized to `## Heading` blocks and parsed back on load. A
// legacy or free-form body with no recognized headings falls into Summary.

export interface PostmortemSections {
  summary: string;
  rootCause: string;
  resolution: string;
  actionItems: string;
}

export const PM_SECTIONS: { key: keyof PostmortemSections; heading: string; placeholder: string }[] = [
  { key: "summary", heading: "Summary", placeholder: "What happened, in a sentence or two." },
  { key: "rootCause", heading: "Root cause", placeholder: "Why it happened." },
  { key: "resolution", heading: "Resolution", placeholder: "How it was fixed." },
  { key: "actionItems", heading: "Action items", placeholder: "- Prevent recurrence…" },
];

export function emptySections(): PostmortemSections {
  return { summary: "", rootCause: "", resolution: "", actionItems: "" };
}

export function serializePostmortem(s: PostmortemSections): string {
  return PM_SECTIONS.filter((sec) => s[sec.key].trim())
    .map((sec) => `## ${sec.heading}\n\n${s[sec.key].trim()}`)
    .join("\n\n");
}

export function parsePostmortem(body?: string): PostmortemSections {
  const out = emptySections();
  if (!body) return out;
  const parts = body.split(/^##\s+/m);
  let matched = false;
  for (const part of parts) {
    const nl = part.indexOf("\n");
    const heading = (nl === -1 ? part : part.slice(0, nl)).trim().toLowerCase();
    const content = (nl === -1 ? "" : part.slice(nl + 1)).trim();
    const sec = PM_SECTIONS.find((s) => s.heading.toLowerCase() === heading);
    if (sec) {
      out[sec.key] = content;
      matched = true;
    }
  }
  if (!matched) out.summary = body.trim(); // legacy / free-form body
  return out;
}

// Non-empty sections for display, in canonical order.
export function renderSections(body?: string): { heading: string; content: string }[] {
  const s = parsePostmortem(body);
  return PM_SECTIONS.filter((sec) => s[sec.key].trim()).map((sec) => ({ heading: sec.heading, content: s[sec.key].trim() }));
}
