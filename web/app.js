// Both pages render the same state, streamed from /api/events. Phones never
// navigate: whatever the presenter advances to is what they show.
(function () {
  "use strict";

  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

  function stream(onState) {
    let source;
    const connect = () => {
      source = new EventSource("/api/events");
      source.onmessage = (e) => onState(JSON.parse(e.data));
      // EventSource reconnects on its own; this only covers a closed stream.
      source.onerror = () => { if (source.readyState === EventSource.CLOSED) setTimeout(connect, 2000); };
    };
    connect();
  }

  function tallyHTML(s, q, showWinner) {
    const [a, b] = s.tally[q];
    const total = a + b || 1;
    const question = s.questions[q];
    const row = (label, n, opt) => {
      const won = showWinner && s.winners[q] === opt;
      return `<div class="bar ${won ? "won" : ""}">
        <div class="fill" style="width:${Math.round((n / total) * 100)}%"></div>
        <span class="label">${esc(label)}</span><span class="count">${n}</span>
      </div>`;
    };
    return row(question.a, a, "A") + row(question.b, b, "B");
  }

  function progressHTML(s) {
    const pods = s.progress.pods || [];
    const elapsed = s.started ? Math.round((Date.now() - new Date(s.started)) / 1000) : 0;
    const steps = pods.length
      ? pods.map((p) => `<li class="${p.step === "ready" ? "done" : ""}">${esc(p.step)}</li>`).join("")
      : `<li>${s.progress.applied ? "Modelplane is scheduling it" : "Handing it to Modelplane"}</li>`;
    return `<p class="muted">${elapsed}s since the votes closed</p><ul class="steps">${steps}</ul>`;
  }

  function summaryHTML(s) {
    const swapped = s.swapped
      ? `<p class="note">You picked exactly the optimized configuration. To give you something to compare, we're deploying its opposite.</p>`
      : "";
    return `${swapped}<ol class="summary">${(s.summary || []).map((d) => `<li>${esc(d)}</li>`).join("")}</ol>`;
  }

  // ----- audience -----

  function audience(root) {
    let state = null;
    const chats = { audience: [], optimized: [] };
    let target = "audience";

    const vote = (q, option) =>
      fetch("/api/vote", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ question: q, option }) })
        .then((r) => r.json()).then(render);

    function chatHTML() {
      const log = chats[target].map((m) => `<div class="msg ${m.role}">${esc(m.content)}${m.meta ? `<span class="meta">${esc(m.meta)}</span>` : ""}</div>`).join("");
      return `<div class="tabs">
          <button data-target="audience" class="${target === "audience" ? "on" : ""}">Your pick</button>
          <button data-target="optimized" class="${target === "optimized" ? "on" : ""}">Optimized</button>
        </div>
        <div class="log">${log || `<p class="muted">Ask either one something, then ask the other.</p>`}</div>
        <form class="ask"><input name="q" placeholder="Ask something" autocomplete="off" maxlength="500"><button>Send</button></form>`;
    }

    function render(s) {
      if (s) state = s;
      s = state;
      if (!s) return;
      let html = "";
      if (s.phase === "lobby") {
        html = `<h1>You're in.</h1><p class="muted">The first question opens in a moment. Keep this page open.</p>`;
      } else if (s.phase === "voting") {
        const q = s.questions[s.question];
        const mine = s.mine[s.question];
        html = `<p class="step">Question ${s.question + 1} of ${s.questions.length}</p><h1>${esc(q.text)}</h1>
          <button class="choice ${mine === "A" ? "picked" : ""}" data-vote="A">${esc(q.a)}</button>
          <button class="choice ${mine === "B" ? "picked" : ""}" data-vote="B">${esc(q.b)}</button>
          <p class="muted">${mine ? "Counted. You can change it until the question closes." : "Go with your gut."}</p>`;
      } else if (s.phase === "results") {
        html = `<p class="step">Question ${s.question + 1} of ${s.questions.length}</p><h1>${esc(s.questions[s.question].text)}</h1>${tallyHTML(s, s.question, true)}`;
      } else if (s.phase === "deploying") {
        html = `<h1>Deploying your pick</h1>${summaryHTML(s)}${progressHTML(s)}`;
      } else if (s.phase === "ready") {
        html = `<h1>Both are serving</h1>${chatHTML()}`;
      }
      root.innerHTML = html;
      root.querySelectorAll("[data-vote]").forEach((b) => (b.onclick = () => vote(s.question, b.dataset.vote)));
      root.querySelectorAll("[data-target]").forEach((b) => (b.onclick = () => { target = b.dataset.target; render(); }));
      const form = root.querySelector("form.ask");
      if (form) form.onsubmit = (e) => { e.preventDefault(); ask(form.q.value.trim()); };
    }

    function ask(text) {
      if (!text) return;
      const which = target;
      const log = chats[which];
      log.push({ role: "user", content: text });
      render();
      const messages = log.filter((m) => m.role !== "error").map(({ role, content }) => ({ role, content }));
      fetch("/api/chat", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ target: which, messages }) })
        .then(async (r) => {
          if (!r.ok) throw new Error(await r.text());
          return r.json();
        })
        .then((a) => log.push({ role: "assistant", content: a.content, meta: `${a.seconds.toFixed(1)}s, ${a.tokens} tokens` }))
        .catch((e) => log.push({ role: "error", content: e.message.trim() }))
        .finally(() => render());
    }

    // The deploying view counts seconds, so it needs a tick between events.
    setInterval(() => state && state.phase === "deploying" && render(), 1000);
    stream(render);
  }

  // ----- presenter -----

  function presenter(root) {
    const token = document.getElementById("token");
    const phase = document.getElementById("phase");
    const next = document.getElementById("next");
    const reset = document.getElementById("reset");
    let state = null;
    let armed = false;

    try { token.value = localStorage.getItem("presenter-token") || ""; } catch (_) {}
    token.onchange = () => { try { localStorage.setItem("presenter-token", token.value); } catch (_) {} };

    const post = (path) =>
      fetch(path, { method: "POST", headers: { Authorization: "Bearer " + token.value } })
        .then((r) => { if (r.status === 401) alertBar("Wrong presenter token"); });

    function alertBar(text) {
      phase.textContent = text;
      phase.classList.add("bad");
    }

    next.onclick = () => post("/api/presenter/next");
    // Reset needs two clicks, so a stray one mid-talk cannot wipe the votes.
    reset.onclick = () => {
      if (!armed) { armed = true; reset.textContent = "Really reset?"; setTimeout(() => { armed = false; reset.textContent = "Reset"; }, 3000); return; }
      armed = false; reset.textContent = "Reset";
      post("/api/presenter/reset");
    };
    // Right arrow and space advance, like a slide clicker.
    document.addEventListener("keydown", (e) => {
      if (document.activeElement === token) return;
      if (e.key === "ArrowRight" || e.key === " " || e.key === "PageDown") { e.preventDefault(); next.click(); }
    });

    function render(s) {
      if (s) state = s;
      s = state;
      if (!s) return;
      phase.classList.remove("bad");
      const labels = { lobby: "Lobby", voting: `Question ${s.question + 1} open`, results: `Question ${s.question + 1} results`, deploying: "Deploying", ready: "Ready" };
      phase.textContent = labels[s.phase] || s.phase;
      next.disabled = s.phase === "deploying" || s.phase === "ready";
      next.textContent = s.phase === "lobby" ? "Open question 1" : s.phase === "voting" ? "Close question" : s.phase === "results" && s.question === s.questions.length - 1 ? "Apply" : "Next question";
      let html = "";
      if (s.phase === "lobby") {
        html = `<h1>Scan the QR code</h1><img class="qr" src="/qr.png" alt="QR code for ${esc(location.host)}"><p class="muted">${esc(location.host)}</p>`;
      } else if (s.phase === "voting" || s.phase === "results") {
        const votes = s.tally[s.question][0] + s.tally[s.question][1];
        html = `<p class="step">Question ${s.question + 1} of ${s.questions.length} - ${votes} votes</p><h1>${esc(s.questions[s.question].text)}</h1>${tallyHTML(s, s.question, s.phase === "results")}`;
      } else {
        html = `<h1>${s.phase === "ready" ? "Both are serving" : "Deploying " + esc(s.deployed)}</h1>
          <p class="muted">Audience picked ${esc(s.audience)}, optimized is ${esc(s.optimized)}</p>${summaryHTML(s)}${s.phase === "deploying" ? progressHTML(s) : ""}`;
      }
      root.innerHTML = html;
    }

    setInterval(() => state && state.phase === "deploying" && render(), 1000);
    stream(render);
  }

  window.InferenceVote = { audience, presenter };
})();
