// docs/adr/0086 §2: the session's stored Rule of Two letters as the label the console shows: "B·C", "A·B", or
// "supervised" for all three. "" (a row from before the field) shows nothing.
export function trustLabel(set: string): string {
  if (set === "ABC") return "supervised";
  return set.split("").join("·");
}
