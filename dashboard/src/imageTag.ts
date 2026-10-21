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

// The release that built a pod, then its image tag: "6.22.1 (src-8a19aa40)".
// Images are tagged src-<input hash> and reused across releases, so the tag
// alone no longer names the release. A pod from before the provisioner
// reported one shows the tag only. Mirrors core's podVersion
// (core/internal/dashboard/metrics.go), which formats the topology cells.
export function podVersion(release: string | undefined, image: string): string {
  let tag = imageTag(image);
  if (tag.startsWith("src-") && tag.length > 12) tag = tag.slice(0, 12);
  if (!release) return tag;
  if (!tag) return release;
  return `${release} (${tag})`;
}
