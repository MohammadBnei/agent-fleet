import { useEffect } from "react";
import { useLocalStorageState } from "./useLocalStorageState";

export type FeedWidth = "760" | "1100" | "full";

// The feed's prose measure. 760px is the readability cap the console shipped
// with — past ~90 characters the eye loses the line on a wrap — but it also
// applied to code blocks, tables and diagrams, which have no such limit and
// were being squeezed while the column had room to spare. Rather than pick a
// second number, it's a choice.
//
// `none` is a valid max-width, so "full" needs no branch at the call sites.
const MEASURE: Record<FeedWidth, string> = {
  "760": "760px",
  "1100": "1100px",
  full: "none",
};

// A CSS custom property rather than a prop: the two capped elements sit in
// SessionFeed and TranscriptEntryView, and threading a width through the feed's
// render loop to reach them would touch every entry kind for a value none of
// them decide. Same shape as useTheme's data-theme attribute.
//
// ponytail: no pre-paint script in index.html (which useTheme needs). A theme
// flash is glaring; a width flash is one frame of wider text. The `:root`
// default in index.css covers first paint.
export function useFeedWidth() {
  const [width, setWidth] = useLocalStorageState<FeedWidth>("herd.feedWidth", "760");
  useEffect(() => {
    document.documentElement.style.setProperty("--feed-measure", MEASURE[width]);
  }, [width]);
  return [width, setWidth] as const;
}
