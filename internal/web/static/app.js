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

  // "Generate" fills a secret field with a random value and shows it, so it
  // can be copied into the GitHub App's settings before saving.
  document.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-generate]");
    if (!btn) return;
    const field = document.getElementById(btn.dataset.generate);
    const bytes = crypto.getRandomValues(new Uint8Array(24));
    field.value = [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
    field.type = "text";
    field.select();
  });

  // Confirmation for destructive forms.
  document.addEventListener("submit", (e) => {
    const msg = e.target.dataset.confirm;
    if (msg && !confirm(msg)) e.preventDefault();
  });

  // Drag-and-drop upload: dropping a file anywhere on a page with a
  // [data-drop] form puts it in the form's file input and submits it.
  const dropForm = document.querySelector("form[data-drop]");
  if (dropForm) {
    const input = dropForm.querySelector('input[type="file"]');
    const status = dropForm.querySelector(".drop-status");
    const accept = (input.accept || "").split(",").map((s) => s.trim().toLowerCase()).filter(Boolean);
    const say = (msg, cls) => { if (status) { status.textContent = msg; status.className = "drop-status result " + (cls || ""); } };
    const hasFiles = (e) => e.dataTransfer && [...e.dataTransfer.types].includes("Files");
    let depth = 0; // dragenter/dragleave fire for every child element
    document.addEventListener("dragenter", (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      if (depth++ === 0) document.body.classList.add("dragging");
    });
    document.addEventListener("dragleave", (e) => {
      if (!hasFiles(e)) return;
      if (--depth <= 0) { depth = 0; document.body.classList.remove("dragging"); }
    });
    // Without this the browser opens or downloads the dropped file instead.
    document.addEventListener("dragover", (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = "copy";
    });
    document.addEventListener("drop", (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      depth = 0;
      document.body.classList.remove("dragging");
      const files = e.dataTransfer.files;
      if (files.length !== 1) {
        say(files.length ? "Drop one file at a time." : "That isn't a file. Zip the skill folder and drop the .zip.", "bad");
        return;
      }
      const name = files[0].name.toLowerCase();
      if (accept.length && !accept.some((ext) => name.endsWith(ext))) {
        say(`${files[0].name} isn't a ${accept.join(" or ")} file. Zip the skill folder and drop the .zip.`, "bad");
        return;
      }
      input.files = files;
      say(`Uploading ${files[0].name}…`);
      dropForm.requestSubmit(); // runs the data-confirm prompt like a normal submit
      if (!dropForm.dataset.submitting) say(""); // cancelled at the prompt
    });
    // On document, after the data-confirm listener, so a cancelled prompt is seen.
    document.addEventListener("submit", (e) => {
      if (e.target !== dropForm || e.defaultPrevented) return;
      dropForm.dataset.submitting = "1";
      dropForm.querySelector("button").disabled = true;
      if (input.files.length) say(`Uploading ${input.files[0].name}…`);
    });
  }

  // Dashboard auto-refresh (paused while the tab is hidden).
  const secs = Number(document.body.dataset.refresh || 0);
  if (secs > 0) {
    setInterval(() => { if (!document.hidden) location.reload(); }, secs * 1000);
  }
})();
