// archivis UI behaviour: face selection and people actions.
(function () {
  async function post(url, body) {
    const r = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {}) });
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || r.statusText);
    return j;
  }
  function scopeOf(el) { return el.closest(".picker-scope") || document; }
  function selected(scope) {
    const ids = [...scope.querySelectorAll(".pick:checked")].map((c) => +c.value);
    const extra = scope.dataset && scope.dataset.include;
    if (extra) ids.push(+extra);
    return ids;
  }
  function status(scope, msg) {
    const s = scope.querySelector(".status") || document.querySelector(".status");
    if (s) s.textContent = msg;
  }

  document.addEventListener("click", async (e) => {
    const b = e.target.closest("[data-action]");
    if (!b) return;
    const scope = scopeOf(b);
    const act = b.dataset.action;
    try {
      if (act === "select-all" || act === "select-none") {
        scope.querySelectorAll(".pick").forEach((c) => (c.checked = act === "select-all"));
      } else if (act === "assign" || act === "confirm") {
        const name = act === "confirm" ? b.dataset.name : (scope.querySelector(".assign-name") || {}).value;
        const faces = selected(scope);
        if (!name || !name.trim()) return status(scope, "Type a name first.");
        if (!faces.length) return status(scope, "Select at least one face.");
        status(scope, "Saving…");
        const r = await post("/api/assign", { name: name.trim(), faces, match: true });
        if (r.auto_error) {
          // labels were saved; stay so the warning can be read
          status(scope, `Labelled ${r.assigned} face(s), but automatic matching failed: ${r.auto_error}`);
          return;
        }
        status(scope, `Labelled ${r.assigned} face(s)` + (r.auto ? `, found ${r.auto} more automatically` : "") + ".");
        setTimeout(() => (location.href = "/person/" + r.person), 700);
      } else if (act === "unassign") {
        const faces = [...scope.querySelectorAll(".pick:checked")].map((c) => +c.value);
        if (!faces.length) return status(scope, "Select faces to unlabel.");
        await post("/api/unassign", { faces });
        location.reload();
      } else if (act === "rename") {
        const name = prompt("New name (an existing name merges the two people):");
        if (!name) return;
        const r = await post(`/api/person/${b.dataset.person}/rename`, { name });
        location.href = "/person/" + r.person;
      } else if (act === "delete-person") {
        if (!confirm("Delete this person? Their faces become unlabelled; photos are not touched.")) return;
        await post(`/api/person/${b.dataset.person}/delete`);
        location.href = "/people";
      } else if (act === "match") {
        status(document, "Matching…");
        const r = await post("/api/match");
        status(document, `Labelled ${r.auto} faces automatically.`);
        setTimeout(() => location.reload(), 800);
      }
    } catch (err) {
      status(scope, "Error: " + err.message);
    }
  });

  // aesthetic feedback on the photo page
  document.querySelectorAll(".aesthetic-feedback").forEach((f) =>
    f.addEventListener("click", async (e) => {
      const b = e.target.closest("[data-verdict],[data-rating]");
      if (!b) return;
      const msg = f.querySelector(".fb-status");
      const body = b.dataset.rating ? { rating: +b.dataset.rating } : { verdict: b.dataset.verdict };
      try {
        await post(`/api/photo/${f.dataset.photo}/aesthetic`, body);
        f.querySelectorAll("[aria-pressed]").forEach((x) => x.removeAttribute("aria-pressed"));
        b.setAttribute("aria-pressed", "true");
        msg.textContent = "Saved: " + (b.dataset.rating ? `${b.dataset.rating} / 10` : b.textContent.toLowerCase()) +
          ". It will help train the next aesthetic model.";
      } catch (err) {
        msg.textContent = "Not saved: " + err.message;
      }
    })
  );

  // per-face naming on the photo page
  document.querySelectorAll(".inline-assign").forEach((f) =>
    f.addEventListener("submit", async (e) => {
      e.preventDefault();
      const name = f.querySelector("input").value.trim();
      if (!name) return;
      try {
        const r = await post("/api/assign", { name, faces: [+f.dataset.face], match: true });
        if (r.auto_error) alert(`Saved, but automatic matching failed: ${r.auto_error}`);
        location.reload();
      } catch (err) {
        alert(err.message);
      }
    })
  );

})();
