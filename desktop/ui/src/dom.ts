/**
 * Minimal DOM builder for the control app UI (issue #181). The webview is
 * plain TS + DOM by design (§20.4: the TS skillset, no framework budget); this
 * is the only rendering helper the views need.
 */
export type Child = Node | string | null | undefined | Child[];

export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  attrs: Record<string, string | number | boolean | ((ev: Event) => void)> = {},
  ...children: Child[]
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (typeof v === "function") {
      // Call sites pass DOM-property names ("onclick"); addEventListener needs
      // the event type ("click") — strip the "on" prefix, lowercase for onDOMActivate-style oddities.
      const type = k.startsWith("on") ? k.slice(2).toLowerCase() : k;
      node.addEventListener(type, v as EventListener);
    } else if (k === "class") node.className = String(v);
    else if (k === "value") (node as HTMLInputElement).value = String(v);
    else if (v !== false) node.setAttribute(k, String(v));
  }
  for (const c of children.flat(9)) {
    if (c === null || c === undefined) continue;
    node.append(typeof c === "string" ? document.createTextNode(c) : (c as Node));
  }
  return node;
}

/** Replace all children of `host` with `next`. */
export function render(host: HTMLElement, ...next: Child[]): void {
  host.replaceChildren(
    ...next
      .flat(9)
      .filter((c): c is Node | string => c !== null && c !== undefined)
      .map((c) => (typeof c === "string" ? document.createTextNode(c) : c)),
  );
}

/** ISO ts → HH:MM:SS for log rows (local time, daemon renders UTC). */
export function clock(ts?: string): string {
  if (!ts) return "--:--:--";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return "--:--:--";
  return d.toLocaleTimeString([], { hour12: false });
}

/** ms → compact duration ("45s", "3m12s", "1h04m"). */
export function dur(ms: number): string {
  if (ms < 1_000) return "";
  const s = Math.floor(ms / 1_000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m${String(s % 60).padStart(2, "0")}s`;
  return `${Math.floor(m / 60)}h${String(m % 60).padStart(2, "0")}m`;
}
