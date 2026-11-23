// A model id that does not start with "claude-" goes through the gateway; the
// rule core uses, so the console holds no list of its own.
export type ModelRoute = "subscription" | "gateway";

export function routeOf(model: string | null | undefined): ModelRoute | "" {
  if (!model) return "";
  return model.startsWith("claude-") ? "subscription" : "gateway";
}

// The result line's own `route` wins; the model id is the fallback for a
// result written before the worker stamped it.
export function resultRoute(r: { route?: string; model?: string }): string {
  return r.route || routeOf(r.model);
}
