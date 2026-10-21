package auth

import "net/http"

// donePage is where the island's sign-in popup lands (docs/adr/0068 §3):
// /auth/login?return_to=/auth/done. It closes itself; the island retries when
// its own window regains focus, so nothing here talks to the opener.
const donePage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Signed in</title>` +
	`<p style="font-family:system-ui,sans-serif;padding:24px">Signed in. You can close this window.</p>` +
	`<script>window.close()</script>`

// Done serves the static landing page. It holds no data, so the Gate exempts it.
func Done(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(donePage))
}
