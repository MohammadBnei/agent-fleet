// One relative-time formatter for the console. There were four — the mobile
// list, Files, Schedules and the blocked-for label — rounding differently
// (floor vs round), stopping at different units ("26h" vs "1d"), and one of
// them saying "yesterday". The same timestamp read three ways across views.
//
// Past: "just now", "5m ago", "3h ago", "2d ago". Future: "in 5m".
// `bare` drops the "ago"/"in" for a duration ("blocked · 5m").
// null for a missing or unparseable timestamp; callers pick their own blank.
export function relativeTime(iso: string | undefined | null, opts: { bare?: boolean; now?: number } = {}): string | null {
  if (!iso) return null;
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return null;
  const ms = (opts.now ?? Date.now()) - t;
  const mins = Math.floor(Math.abs(ms) / 60_000);
  if (mins < 1) return ms >= 0 || opts.bare ? "just now" : "in <1m";
  const unit = mins < 60 ? `${mins}m` : mins < 1440 ? `${Math.floor(mins / 60)}h` : `${Math.floor(mins / 1440)}d`;
  if (opts.bare) return unit;
  return ms >= 0 ? `${unit} ago` : `in ${unit}`;
}
