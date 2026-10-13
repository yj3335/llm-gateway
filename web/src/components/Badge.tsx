import type { Tone } from "@/lib/format";

const toneClasses: Record<Tone, string> = {
  ok: "bg-ok/15 text-ok ring-ok/30",
  warn: "bg-warn/15 text-warn ring-warn/30",
  bad: "bg-bad/15 text-bad ring-bad/30",
  info: "bg-info/15 text-info ring-info/30",
  muted: "bg-faint/15 text-dim ring-faint/30",
};

export function Badge({ tone, children, testId }: { tone: Tone; children: React.ReactNode; testId?: string }) {
  return (
    <span
      data-testid={testId}
      data-tone={tone}
      className={`inline-flex items-center whitespace-nowrap rounded px-1.5 py-0.5 font-mono text-[11px] font-medium ring-1 ring-inset ${toneClasses[tone]}`}
    >
      {children}
    </span>
  );
}

export function Panel({ title, subtitle, children, right, testId }: { title: string; subtitle?: string; children: React.ReactNode; right?: React.ReactNode; testId?: string }) {
  return (
    <section data-testid={testId} className="rounded-lg border border-line bg-panel">
      <header className="flex items-start justify-between gap-4 border-b border-line px-4 py-3">
        <div>
          <h2 className="text-sm font-semibold text-ink">{title}</h2>
          {subtitle ? <p className="mt-0.5 text-xs text-dim">{subtitle}</p> : null}
        </div>
        {right}
      </header>
      <div className="p-4">{children}</div>
    </section>
  );
}
