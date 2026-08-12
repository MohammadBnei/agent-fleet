import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./index.css";
import App from "./App.tsx";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);

// Production only: under `vite dev` a service worker caches module output that
// HMR is simultaneously replacing, which produces stale-module bugs that look
// like application bugs. Registration failing is not worth surfacing — the app
// works identically without it, it just isn't installable.
if (import.meta.env.PROD && "serviceWorker" in navigator) {
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch(() => {});
  });
}
