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

  // 0:07, 4:41, 12:00
  function clock(seconds) {
    const s = Math.max(0, Math.floor(seconds));
    return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
  }
  const since = (t) => (t ? (Date.now() - new Date(t)) / 1000 : 0);

  // What to call each side. When the room picks exactly the optimized config,
  // that side is their pick, and the other is its opposite.
  const names = (s) => (s && s.swapped
    ? { optimized: "Your pick", audience: "Unoptimized" }
    : { optimized: "Optimized", audience: "Your pick" });

  function deployingHTML(s) {
    const head = s.swapped
      ? `<h1>Deploying the opposite of your pick</h1><p class="note">You picked exactly the optimized configuration. It's already running, so the opposite of every answer races it.</p>`
      : `<h1>Deploying your pick</h1>`;
    return `${head}${summaryHTML(s.summary)}${progressHTML(s)}`;
  }

  function tallyHTML(s, q) {
    const [a, b] = s.tally[q];
    const total = a + b || 1;
    const question = s.questions[q];
    const row = (label, n) => `<div class="bar">
        <div class="fill" style="width:${Math.round((n / total) * 100)}%"></div>
        <span class="label">${esc(label)}</span><span class="count">${n}</span>
      </div>`;
    return row(question.a, a) + row(question.b, b);
  }

  // One line per engine pod: two copies show as two lines, so name them.
  function podsHTML(pods, applied) {
    if (!pods || !pods.length) {
      return `<ul class="steps"><li>${applied ? "Modelplane is scheduling it" : "Handing it to Modelplane"}</li></ul>`;
    }
    return `<ul class="steps">${pods.map((p, i) => {
      const who = pods.length > 1 ? `Copy ${i + 1}` : "Engine";
      const crashed = p.restarts
        ? `<span class="crash">crashed ${p.restarts} ${p.restarts === 1 ? "time" : "times"}${p.reason ? `: ${esc(p.reason)}` : ""}</span>`
        : "";
      return `<li class="${p.step === "ready" ? "done" : p.restarts ? "bad" : ""}">${who}: ${esc(p.step)}${crashed}</li>`;
    }).join("")}</ul>`;
  }

  // Ready engines still need Modelplane's gateway route before anyone can
  // reach them, which can take minutes; say so, or it looks stuck.
  function gatewayHTML(s) {
    if (!s.enginesReady) return "";
    return `<ul class="steps"><li class="${s.serving ? "done" : ""}">Gateway: ${s.serving ? "answering" : "waiting for the route"}</li></ul>`;
  }

  function progressHTML(s) {
    return `<p class="muted">${clock(since(s.started))} since the votes closed</p>${podsHTML(s.progress.pods, s.progress.applied)}${gatewayHTML(s)}`;
  }

  function summaryHTML(summary) {
    return `<ol class="summary">${(summary || []).map((d) => `<li>${esc(d)}</li>`).join("")}</ol>`;
  }

  // ----- audience -----

  function audience(root) {
    let state = null;
    const chats = { audience: [], optimized: [] };
    let target = "audience";

    const vote = (q, option) =>
      fetch("/api/vote", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ question: q, option }) })
        .then((r) => r.json()).then(render);

    // The chat is drawn once. After that only the messages and the "Your pick"
    // tab change, in place: redrawing it would replace the field someone is
    // typing in, and close their keyboard.
    function logHTML() {
      const log = chats[target].map((m) => `<div class="msg ${m.role}">${esc(m.content)}${m.meta ? `<span class="meta">${esc(m.meta)}</span>` : ""}</div>`).join("");
      return log || `<p class="muted">Ask either one something, then ask the other.</p>`;
    }

    function updateChat(s) {
      if (!s.serving && target === "audience") target = "optimized";
      const mine = root.querySelector('[data-target="audience"]');
      mine.disabled = !s.serving;
      root.querySelectorAll("[data-target]").forEach((b) => b.classList.toggle("on", b.dataset.target === target));
      root.querySelector(".not-serving").hidden = s.serving;
      root.querySelector(".log").innerHTML = logHTML();
    }

    function drawChat(s) {
      const n = names(s);
      root.innerHTML = `<h1>Chat with them</h1>
        <div class="tabs">
          <button data-target="audience">${n.audience}</button>
          <button data-target="optimized">${n.optimized}</button>
        </div>
        <p class="muted not-serving">${s.swapped ? "The unoptimized one isn't serving, so only your pick can answer." : "Your pick isn't serving, so only the optimized one can answer."}</p>
        <div class="log"></div>
        <form class="ask"><input name="q" placeholder="Ask something" autocomplete="off" maxlength="500"><button>Send</button></form>`;
      root.dataset.view = "race";
      root.querySelectorAll("[data-target]").forEach((b) => (b.onclick = () => { target = b.dataset.target; updateChat(state); }));
      const form = root.querySelector("form.ask");
      form.onsubmit = (e) => { e.preventDefault(); const text = form.q.value.trim(); form.q.value = ""; ask(text); };
      updateChat(s);
    }

    function render(s) {
      if (s) state = s;
      s = state;
      if (!s) return;
      if (s.phase === "race") {
        if (root.dataset.view === "race") updateChat(s);
        else drawChat(s);
        return;
      }
      root.dataset.view = s.phase;
      let html = "";
      if (s.phase === "lobby") {
        html = `<h1>You're in.</h1><p class="muted">The first question opens in a moment. Keep this page open.</p>`;
      } else if (s.phase === "voting") {
        const q = s.questions[s.question];
        const mine = s.mine[s.question];
        html = mine
          ? `<p class="step">Question ${s.question + 1} of ${s.questions.length}</p><h1>Voted</h1>
            <p class="muted">You picked: ${esc(mine === "A" ? q.a : q.b)}</p>
            <p class="muted">The next question opens here on its own.</p>`
          : `<p class="step">Question ${s.question + 1} of ${s.questions.length}</p><h1>${esc(q.text)}</h1>
            <button class="choice" data-vote="A">${esc(q.a)}</button>
            <button class="choice" data-vote="B">${esc(q.b)}</button>
            <p class="muted">Go with your gut.</p>`;
      } else if (s.phase === "deploying") {
        html = deployingHTML(s);
      }
      root.innerHTML = html;
      root.querySelectorAll("[data-vote]").forEach((b) => (b.onclick = () => vote(s.question, b.dataset.vote)));
    }

    function ask(text) {
      if (!text) return;
      const which = target;
      const log = chats[which];
      log.push({ role: "user", content: text });
      updateChat(state);
      const messages = log.filter((m) => m.role !== "error").map(({ role, content }) => ({ role, content }));
      fetch("/api/chat", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ target: which, messages }) })
        .then(async (r) => {
          if (!r.ok) throw new Error(await r.text());
          return r.json();
        })
        .then((a) => log.push({ role: "assistant", content: a.content, meta: `${a.seconds.toFixed(1)}s, ${a.tokens} tokens` }))
        .catch((e) => log.push({ role: "error", content: e.message.trim() }))
        .finally(() => updateChat(state));
    }

    // The deploying view counts time, so it needs a tick between events.
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
    let race = null;
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

    // In the race, Next asks for the results so far; again refreshes them.
    next.onclick = () => post(state && state.phase === "race" ? "/api/presenter/results" : "/api/presenter/next");
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

    function sideHTML(title, side, s, isAudience) {
      const l = side.load;
      const a = side.accuracy;
      let body;
      if (!side.serving) {
        const crashed = (side.pods || []).some((p) => p.restarts);
        body = `<p class="${crashed ? "crash" : "muted"}">${crashed ? "Not serving." : "Not serving yet."}</p>${podsHTML(side.pods, true)}${gatewayHTML(s)}`;
      } else {
        const num = (v, digits) => (v === undefined || v === null ? "-" : v.toFixed(digits));
        body = `<table class="stats">
          <tr><th>Load-test requests</th><td>${l ? l.requests : "-"}${l && l.failures ? ` <span class="crash">(${l.failures} failed)</span>` : ""}</td></tr>
          <tr><th>Audience messages</th><td>${side.chats}</td></tr>
          <tr><th>Tokens per second</th><td>${l ? num(l.tokensPerSec, 0) : "-"}</td></tr>
          <tr><th>Answer time, median</th><td>${l && l.p50 ? num(l.p50, 1) + "s" : "-"}</td></tr>
          <tr><th>Answer time, slowest 5%</th><td>${l && l.p95 ? num(l.p95, 1) + "s" : "-"}</td></tr>
          <tr><th>Math accuracy</th><td>${a && a.answered ? `${(100 * a.correct / a.answered).toFixed(1)}% <span class="muted">(${a.correct}/${a.answered}${a.done ? "" : ` of ${a.total}`})</span>` : "-"}</td></tr>
        </table>
        ${l ? `<p class="muted">${clock(l.seconds)}${l.done ? ", done" : ""}</p>` : ""}`;
      }
      const swapped = !s.swapped ? ""
        : isAudience ? `<p class="note">Every answer flipped, to show what the choices are worth.</p>`
        : `<p class="note">The room picked exactly the optimized configuration.</p>`;
      // Four parts, one per row of the shared grid, so both columns line up
      // whichever has the longer answers.
      return `<section class="side"><h2>${esc(title)} <span class="config">${esc(side.config)}</span></h2><div>${swapped}${summaryHTML(side.summary)}</div><div>${body}</div></section>`;
    }

    // The code decides each winner; the optimized model explains why, from
    // those numbers and what the engines reported.
    function resultsHTML(r, n) {
      if (!r) return "";
      const mark = (row, who) => (row.winner === who ? ` class="win"` : "");
      const table = r.rows && r.rows.length
        ? `<table class="results"><thead><tr><th></th><th>${n.optimized}</th><th>${n.audience}</th></tr></thead><tbody>${r.rows.map((row) =>
            `<tr><th>${esc(row.measure)}</th><td${mark(row, "optimized")}>${esc(row.optimized)}</td><td${mark(row, "audience")}>${esc(row.audience)}</td></tr>`).join("")}</tbody></table>`
        : "";
      const pct = (v) => (v === null || v === undefined ? "-" : `${Math.round(v * 100)}%`);
      const o = (r.evidence || {}).optimized, a = (r.evidence || {}).audience;
      const evidence = o && a
        ? `<table class="results evidence"><thead><tr><th>What the engines reported</th><th>${n.optimized}</th><th>${n.audience}</th></tr></thead><tbody>
            <tr><th>Room for full-length conversations, per engine</th><td>${o.maxConcurrency.toFixed(1)}</td><td>${a.maxConcurrency.toFixed(1)}</td></tr>
            <tr><th>KV cache in use, average</th><td>${pct(o.kvUsageAvg)}</td><td>${pct(a.kvUsageAvg)}</td></tr>
            <tr><th>Requests waiting, average</th><td>${Math.round(o.waitingAvg)}</td><td>${Math.round(a.waitingAvg)}</td></tr>
            <tr><th>Conversations evicted</th><td>${Math.round(o.preemptions)}</td><td>${Math.round(a.preemptions)}</td></tr>
            <tr><th>Prompt found in the prefix cache</th><td>${pct(o.prefixHitRate)}</td><td>${pct(a.prefixHitRate)}</td></tr>
            <tr><th>Guessed tokens accepted</th><td>${pct(o.specAcceptance)}</td><td>${pct(a.specAcceptance)}</td></tr>
          </tbody></table>`
        : "";
      const by = r.source === "writing"
        ? (r.text ? "The optimized model is writing..." : "Asking the optimized model, which is busy with the load test...")
        : r.source === "model" ? "Written by the optimized model"
        : "The model didn't answer in time; this is from the numbers alone";
      return `<section class="verdict">
        <h2>${r.final ? "Results" : `Results after ${clock(r.elapsed)}`}</h2>
        <div class="verdict-grid"><div>${table}${evidence}</div>
        <div><p class="analysis">${esc(r.text || "")}</p><p class="muted">${by}</p></div></div>
      </section>`;
    }

    // Same line, same height: lines only one manifest has leave a gap on the
    // other side, and what differs is marked.
    function manifestsHTML(rows, n) {
      // A long line wraps under its own start, not back at the left edge, so
      // the indentation still reads as YAML.
      const cell = (line) => {
        const indent = line.length - line.trimStart().length;
        return `<td><pre style="padding-left:${indent}ch">${esc(line.trimStart())}</pre></td>`;
      };
      return `<table class="manifests"><thead><tr><th>${n.optimized}</th><th>${n.audience}</th></tr></thead><tbody>${rows.map((r) =>
        `<tr class="${r.kind}">${cell(r.left)}${cell(r.right)}</tr>`).join("")}</tbody></table>`;
    }

    function render(s) {
      if (s) state = s;
      s = state;
      if (!s) return;
      phase.classList.remove("bad");
      const labels = { lobby: "Lobby", voting: `Question ${s.question + 1} open`, deploying: "Deploying", race: "Race" };
      phase.textContent = labels[s.phase] || s.phase;
      next.disabled = false;
      next.textContent = s.phase === "lobby" ? "Open question 1"
        : s.phase === "voting" ? (s.question === s.questions.length - 1 ? "Apply" : "Next question")
        : s.phase === "deploying" ? "Start the race"
        : race && race.results ? "Refresh results" : "Show results";
      let html = "";
      if (s.phase === "lobby") {
        html = `<h1>Scan the QR code</h1><img class="qr" src="/qr.png" alt="QR code for ${esc(location.host)}"><p class="muted">${esc(location.host)}</p>`;
      } else if (s.phase === "voting") {
        const votes = s.tally[s.question][0] + s.tally[s.question][1];
        html = `<p class="step">Question ${s.question + 1} of ${s.questions.length} - ${votes} votes</p><h1>${esc(s.questions[s.question].text)}</h1>${tallyHTML(s, s.question)}`;
      } else if (s.phase === "deploying") {
        html = deployingHTML(s);
      } else if (s.phase === "race") {
        const n = names(s);
        html = race
          ? `<h1>The race <span class="muted">${race.ended ? `${clock((new Date(race.ended) - new Date(race.started)) / 1000)}, finished` : clock(since(race.started))}</span></h1>
            <div class="race">${sideHTML(n.optimized, race.optimized, s, false)}${sideHTML(n.audience, race.audience, s, true)}</div>
            ${resultsHTML(race.results, n)}
            ${manifestsHTML(race.manifests, n)}`
          : `<h1>The race</h1><p class="muted">Starting...</p>`;
      }
      // Keep the manifests where the presenter scrolled them.
      const y = window.scrollY;
      root.innerHTML = html;
      window.scrollTo(0, y);
    }

    function pollRace() {
      if (!state || state.phase !== "race") { race = null; return; }
      fetch("/api/race").then((r) => (r.status === 200 ? r.json() : null)).then((r) => { race = r; render(); }).catch(() => {});
    }

    setInterval(() => state && state.phase === "deploying" && render(), 1000);
    setInterval(pollRace, 2000);
    stream((s) => { render(s); if (s.phase === "race" && !race) pollRace(); });
  }

  window.InferenceVote = { audience, presenter };
})();
