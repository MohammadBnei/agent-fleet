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

// The pickable ids in two labelled groups, in the order core lists them within
// each. The same grouping in the new-session dialog and the model control; an
// empty group is left out (gateway models off hides the second).
export const ROUTE_LABEL: Record<ModelRoute, string> = {
  subscription: "Claude (subscription)",
  gateway: "Gateway (Venice)",
};
export function groupModels(
  models: string[],
): { route: ModelRoute; label: string; models: string[] }[] {
  return (["subscription", "gateway"] as const)
    .map((route) => ({
      route,
      label: ROUTE_LABEL[route],
      models: models.filter((m) => routeOf(m) === route),
    }))
    .filter((g) => g.models.length > 0);
}
