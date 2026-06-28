"use strict";

// Renders a runclaude session transcript from the stream-json event feed.
// Read-only in Phase 2: it subscribes to /events (SSE) and draws each event.
// Phases 3+ enable the composer and the take-control flow.

const transcriptEl = document.getElementById("transcript");
const sessionEl = document.getElementById("session");
const statusDot = document.getElementById("status-dot");

// Per-connection participant id: coordination/attribution only, never the authz
// gate (the server keys eligibility on the resolved Tailscale/dev identity).
// Stable per tab so a reconnect keeps the same control token.
const PID = (() => {
  let v = sessionStorage.getItem("runclaude-pid");
  if (!v) { v = Math.random().toString(36).slice(2) + Date.now().toString(36); sessionStorage.setItem("runclaude-pid", v); }
  return v;
})();

const state = { mayWrite: false, myName: "", controller: null, controllerName: "", wasController: false, caughtUp: false, sessionId: "" };

// In dev mode the identity rides in the query string (?as=); forward it on
// every request so the server resolves the same principal.
const qs = window.location.search;

function setStatus(state) {
  statusDot.className = "dot " + state;
  statusDot.title = state;
}

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
}

// --- ANSI (SGR) -> HTML, for the colored jj source views ---
const ANSI16 = ["#000000","#cd3131","#0dbc79","#e5e510","#2472c8","#bc3fbc","#11a8cd","#e5e5e5",
                "#666666","#f14c4c","#23d18b","#f5f543","#3b8eea","#d670d6","#29b8db","#ffffff"];
function rgbHex(r,g,b){ return "#"+[r,g,b].map(x=>(x&255).toString(16).padStart(2,"0")).join(""); }
function xterm256(n){
  if (n < 16) return ANSI16[n];
  if (n <= 231){ n -= 16; const f=v=>v?55+v*40:0; return rgbHex(f((n/36|0)),f(((n/6|0)%6)),f(n%6)); }
  const v = 8 + (n-232)*10; return rgbHex(v,v,v);
}
function escHtml(s){ return s.replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;"); }
// Convert text with ANSI SGR escapes to safe HTML (text is escaped; only our
// own <span style> tags are emitted, so repo content can't inject markup).
function ansiToHtml(input){
  let out="", fg=null, bg=null, bold=false, ul=false;
  const flush = (t) => {
    if (!t) return;
    let st = "";
    if (fg) st += "color:"+fg+";";
    if (bg) st += "background:"+bg+";";
    if (bold) st += "font-weight:600;";
    if (ul) st += "text-decoration:underline;";
    out += st ? `<span style="${st}">${escHtml(t)}</span>` : escHtml(t);
  };
  const re = /\x1b\[([0-9;]*)m/g;
  let last = 0, m;
  while ((m = re.exec(input))){
    flush(input.slice(last, m.index));
    last = re.lastIndex;
    const codes = m[1] === "" ? [0] : m[1].split(";").map(Number);
    for (let i = 0; i < codes.length; i++){
      const c = codes[i];
      if (c === 0){ fg=bg=null; bold=ul=false; }
      else if (c === 1) bold = true;
      else if (c === 22) bold = false;
      else if (c === 4) ul = true;
      else if (c === 24) ul = false;
      else if (c >= 30 && c <= 37) fg = ANSI16[c-30];
      else if (c >= 90 && c <= 97) fg = ANSI16[c-90+8];
      else if (c === 39) fg = null;
      else if (c >= 40 && c <= 47) bg = ANSI16[c-40];
      else if (c >= 100 && c <= 107) bg = ANSI16[c-100+8];
      else if (c === 49) bg = null;
      else if (c === 38 || c === 48){
        const isFg = c === 38, mode = codes[i+1];
        let col = null;
        if (mode === 5){ col = xterm256(codes[i+2]); i += 2; }
        else if (mode === 2){ col = rgbHex(codes[i+2],codes[i+3],codes[i+4]); i += 4; }
        if (isFg) fg = col; else bg = col;
      }
    }
  }
  flush(input.slice(last));
  return out;
}

function atBottom() {
  return transcriptEl.scrollHeight - transcriptEl.scrollTop - transcriptEl.clientHeight < 80;
}
function scrollIfFollowing(wasBottom) {
  if (wasBottom) transcriptEl.scrollTop = transcriptEl.scrollHeight;
}

function addMsg(role, nodes, meta) {
  const wasBottom = atBottom();
  const wrap = el("div", "msg " + role);
  if (meta) wrap.appendChild(el("div", "meta", meta));
  const bubble = el("div", "bubble");
  for (const n of nodes) bubble.appendChild(n);
  wrap.appendChild(bubble);
  transcriptEl.appendChild(wrap);
  scrollIfFollowing(wasBottom);
}

function addBlock(node) {
  const wasBottom = atBottom();
  transcriptEl.appendChild(node);
  scrollIfFollowing(wasBottom);
}

// collapsible renders monospace text clamped to `limit` lines with a toggle to
// expand the rest. Tool inputs and outputs are noisy, so they default collapsed.
function collapsible(text, limit = 3) {
  const wrap = el("div", "collapsible");
  const pre = el("pre");
  const lines = text.split("\n");
  const long = lines.length > limit;
  const head = lines.slice(0, limit).join("\n");
  pre.textContent = long ? head : text;
  wrap.appendChild(pre);
  if (long) {
    let open = false;
    const btn = el("button", "expand");
    const label = () => { btn.textContent = open ? "▾ show less" : `▸ show ${lines.length - limit} more lines`; };
    btn.onclick = () => { open = !open; pre.textContent = open ? text : head; label(); };
    label();
    wrap.appendChild(btn);
  }
  return wrap;
}

function toolCard(name, input) {
  const card = el("div", "tool");
  card.appendChild(el("span", "name", "⚒ " + name));
  if (input && Object.keys(input).length) {
    card.appendChild(collapsible(JSON.stringify(input, null, 2)));
  }
  return card;
}

// A user message content is either a plain string (a typed prompt) or an array
// of blocks; tool_result blocks are the outputs of tool calls.
function renderUser(msg) {
  const content = msg.message && msg.message.content;
  if (typeof content === "string") {
    addMsg("user", [document.createTextNode(content)]);
    return;
  }
  if (Array.isArray(content)) {
    for (const b of content) {
      if (b.type === "tool_result") {
        const card = el("div", "tool");
        card.appendChild(el("span", "name", "↳ result"));
        const text = typeof b.content === "string"
          ? b.content
          : (Array.isArray(b.content) ? b.content.map(c => c.text || "").join("") : "");
        if (text) card.appendChild(collapsible(text));
        addBlock(card);
      } else if (b.type === "text") {
        addMsg("user", [document.createTextNode(b.text)]);
      }
    }
  }
}

function renderAssistant(msg) {
  const content = (msg.message && msg.message.content) || [];
  for (const b of content) {
    if (b.type === "text" && b.text) {
      addMsg("assistant", [document.createTextNode(b.text)]);
    } else if (b.type === "tool_use") {
      addBlock(toolCard(b.name, b.input));
    }
  }
}

function handle(ev) {
  switch (ev.type) {
    case "system":
      if (ev.subtype === "init") {
        state.sessionId = ev.session_id || "";
        sessionEl.textContent = (ev.model ? ev.model + " · " : "") + state.sessionId;
        sessionEl.title = "session " + state.sessionId;
        document.getElementById("copy-session").hidden = !state.sessionId;
      }
      break;
    case "user":
      renderUser(ev);
      break;
    case "assistant":
      renderAssistant(ev);
      break;
    case "result":
      if (ev.is_error) addBlock(el("div", "result", "turn error: " + (ev.subtype || "")));
      break;
    case "control":
      applyControl(ev.controller || null, ev.controllerName || "");
      break;
    case "synced":
      state.caughtUp = true;
      break;
  }
}

// applyControl reconciles the UI with the broadcast control state: who drives,
// whether the composer is live, and a notice when *we* are displaced.
function applyControl(controllerPid, controllerName) {
  const controlEl = document.getElementById("control");
  const iControl = controllerPid && controllerPid === PID;

  if (state.caughtUp && state.wasController && !iControl) {
    addBlock(el("div", "notice", "you lost control" + (controllerName ? " to " + controllerName : "")));
  }
  state.controller = controllerPid;
  state.controllerName = controllerName;
  state.wasController = iControl;

  if (!controllerPid) controlEl.textContent = "no one is driving";
  else if (iControl) controlEl.textContent = "you are driving";
  else controlEl.textContent = controllerName + " is driving";

  document.getElementById("role").textContent = iControl ? "controller" : "read-only";
  document.getElementById("role").classList.toggle("controller", !!iControl);
  document.getElementById("release").hidden = !iControl;
  document.getElementById("take").hidden = !state.mayWrite || iControl;

  const prompt = document.getElementById("prompt");
  const send = document.getElementById("send");
  prompt.disabled = !iControl;
  send.disabled = !iControl;
  document.getElementById("stop").disabled = !iControl;
  prompt.placeholder = iControl ? "Send a prompt…" : "Take control to send a prompt…";
}

async function post(path, body) {
  const res = await fetch(path + qs, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok && res.status !== 204) {
    addBlock(el("div", "notice", path + " failed: " + (await res.text()).trim()));
  }
  return res;
}

function wireControls() {
  document.getElementById("take").onclick = () => post("/take-control", { id: PID, name: state.myName });
  document.getElementById("release").onclick = () => post("/release-control", { id: PID });
  const prompt = document.getElementById("prompt");
  const send = document.getElementById("send");
  const submit = async () => {
    const text = prompt.value.trim();
    if (!text) return;
    const res = await post("/prompt", { id: PID, name: state.myName, text });
    if (res.ok || res.status === 204) prompt.value = "";
  };
  send.onclick = submit;
  prompt.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); submit(); }
  });
  document.getElementById("stop").onclick = () => post("/interrupt", { id: PID });
}

function connect() {
  const src = new EventSource("/events");
  src.onopen = () => setStatus("live");
  src.onerror = () => setStatus("dead"); // EventSource auto-reconnects
  src.onmessage = (e) => {
    let ev;
    try { ev = JSON.parse(e.data); } catch { return; }
    handle(ev);
  };
  // Session ended: stop the automatic reconnect loop and mark the feed done.
  src.addEventListener("end", () => {
    src.close();
    setStatus("dead");
    addBlock(el("div", "notice", "session ended"));
  });
}

// Resolve who the server thinks we are and whether we may take control. In dev
// mode the identity rides in ?as=<login>; forward it so /whoami agrees.
async function loadIdentity() {
  try {
    const res = await fetch("/whoami" + qs);
    const me = await res.json();
    state.myName = me.name || me.login || "";
    state.mayWrite = !!me.mayWrite;
    document.getElementById("me").textContent = state.myName ? "you: " + state.myName : "";
  } catch { /* identity is informational; default to read-only */ }
  // Reconcile button visibility now that mayWrite is known.
  applyControl(state.controller, state.controllerName);
}

// Read-only source panel: polls a jj view (show @ / diff / log / files) while
// open and renders its ANSI color. Inherently read-only, so it sidesteps the
// control gate entirely.
function wireSourcePanel() {
  const toggle = document.getElementById("source-toggle");
  const panel = document.getElementById("source");
  const body = document.getElementById("source-body");
  const select = document.getElementById("source-view");
  let timer = null;
  let filePath = null; // when set, show one file's contents (drill-in from "files")

  async function fetchView() {
    const p = new URLSearchParams(qs ? qs.slice(1) : "");
    p.set("view", filePath ? "file" : select.value);
    if (filePath) p.set("path", filePath);
    return fetch("/source?" + p.toString());
  }

  async function refresh() {
    try {
      const res = await fetchView();
      if (res.status === 404) { body.textContent = "(no jj repo for this session)"; stop(); return; }
      if (!res.ok) { body.textContent = "(source unavailable)"; return; }
      render(await res.text());
    } catch { body.textContent = "(source unavailable)"; }
  }

  function render(text) {
    if (filePath) {
      // One file's contents, with a way back to the list.
      body.replaceChildren();
      const back = el("button", "file-link", "← files");
      back.onclick = () => { filePath = null; refresh(); };
      body.append(back, el("div", "file-name", filePath), document.createTextNode(text || "(empty)"));
    } else if (select.value === "files") {
      // Clickable list of tracked files.
      body.replaceChildren();
      const files = text.split("\n").filter(Boolean);
      if (!files.length) { body.textContent = "(no tracked files)"; return; }
      for (const f of files) {
        const link = el("button", "file-link", f);
        link.onclick = () => { filePath = f; refresh(); };
        body.append(link, document.createTextNode("\n"));
      }
    } else {
      // show / diff / log: render the ANSI color jj emits.
      body.innerHTML = ansiToHtml(text) || "(empty)";
    }
  }

  function stop() { if (timer) { clearInterval(timer); timer = null; } }

  select.onchange = () => { filePath = null; if (!panel.hidden) refresh(); };

  toggle.onclick = () => {
    const show = panel.hidden;
    panel.hidden = !show;
    if (show) { refresh(); timer = setInterval(refresh, 4000); } else { stop(); }
  };

  // Maximize the panel to full width (and back), for reading wide diffs.
  const expand = document.getElementById("source-expand");
  expand.onclick = () => {
    const max = panel.classList.toggle("expanded");
    expand.textContent = max ? "⤡ restore" : "⤢ expand";
  };
}

// Copy the session id (for `runclaude --serve --serve-resume <id>`).
document.getElementById("copy-session").onclick = async (e) => {
  if (!state.sessionId) return;
  try { await navigator.clipboard.writeText(state.sessionId); } catch { /* clipboard may be blocked over http */ }
  const btn = e.target;
  const was = btn.textContent;
  btn.textContent = "copied";
  setTimeout(() => { btn.textContent = was; }, 1200);
};

setStatus("");
wireControls();
wireSourcePanel();
loadIdentity();
connect();
