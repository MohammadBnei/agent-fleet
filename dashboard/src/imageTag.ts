// Reduces a full image ref to the part worth showing: "…/agent-fleet-worker:3.5.4"
// → "3.5.4". Mirrors core's own imageTag (core/internal/dashboard/metrics.go),
// which does this for the topology's worker cells.
//
// The colon is only a tag separator when it comes after the last slash: a
// registry may carry a port ("reg:5000/worker"), and treating that as a tag
// would label the cell with a port number.
export function imageTag(ref: string): string {
  const i = ref.lastIndexOf(":");
  if (i < 0 || i < ref.lastIndexOf("/")) return ref;
  return ref.slice(i + 1);
}
