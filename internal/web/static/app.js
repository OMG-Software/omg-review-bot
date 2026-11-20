// Small progressive enhancements; every page works without JS.
(function () {
  "use strict";

  // Connection tests / model listing: post the current (unsaved) settings form.
  document.addEventListener("click", async (e) => {
    const btn = e.target.closest("[data-test]");
    if (!btn) return;
    e.preventDefault();
    const form = btn.closest("form");
    const out = document.getElementById(btn.dataset.out);
    btn.disabled = true;
    out.className = "result";
    out.textContent = "Working…";
    try {
      const res = await fetch(btn.dataset.test, { method: "POST", body: new FormData(form) });
      const j = await res.json();
      out.textContent = j.message;
      out.className = "result " + (j.ok ? "ok" : "bad");
      if (j.models) {
        const dl = document.getElementById("models");
        dl.replaceChildren(...j.models.map((m) => Object.assign(document.createElement("option"), { value: m })));
      }
    } catch (err) {
      out.textContent = String(err);
      out.className = "result bad";
    } finally {
      btn.disabled = false;
    }
  });

  // Switching provider fills in its default URL unless a custom one was typed.
  const provider = document.getElementById("llm-provider");
  const base = document.getElementById("llm-base");
  if (provider && base) {
    const defaults = new Set([...provider.options].map((o) => o.dataset.base).filter(Boolean));
    provider.addEventListener("change", () => {
      const def = provider.selectedOptions[0].dataset.base;
      if (!base.value || defaults.has(base.value)) base.value = def || "";
    });
  }

  // Confirmation for destructive forms.
  document.addEventListener("submit", (e) => {
    const msg = e.target.dataset.confirm;
    if (msg && !confirm(msg)) e.preventDefault();
  });

  // Dashboard auto-refresh (paused while the tab is hidden).
  const secs = Number(document.body.dataset.refresh || 0);
  if (secs > 0) {
    setInterval(() => { if (!document.hidden) location.reload(); }, secs * 1000);
  }
})();
