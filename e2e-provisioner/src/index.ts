import { startHttpServer } from "./http.js";
import { runReconcileLoop } from "./reconcile.js";
import { log } from "./log.js";

startHttpServer();
runReconcileLoop().catch((err) => {
  log("error", "e2e-provisioner reconcile loop crashed", { error: String(err) });
  process.exit(1);
});
