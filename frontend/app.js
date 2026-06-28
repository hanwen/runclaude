"use strict";

// Renders a runclaude session transcript from the stream-json event feed.
// Read-only in Phase 2: it subscribes to /events (SSE) and draws each event.
// Phases 3+ enable the composer and the take-control flow.

const transcriptEl = document.getElementById("transcript");
const sessionEl = document.getElementById("session");
const statusDot = document.getElementById("status-dot");

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

function toolCard(name, input) {
  const card = el("div", "tool");
  card.appendChild(el("span", "name", "⚒ " + name));
  if (input && Object.keys(input).length) {
    card.appendChild(el("pre", null, JSON.stringify(input, null, 2)));
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
        if (text) card.appendChild(el("pre", null, text));
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
        sessionEl.textContent = (ev.model ? ev.model + " · " : "") + (ev.session_id || "").slice(0, 8);
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
  }
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

// Show who the server thinks we are. In dev mode the identity can be chosen
// with ?as=<login>; forward that query string to /whoami so it agrees.
async function loadIdentity() {
  try {
    const res = await fetch("/whoami" + window.location.search);
    const me = await res.json();
    document.getElementById("me").textContent = me.name || me.login || "";
  } catch { /* identity is informational in Phase 2 */ }
}

setStatus("");
loadIdentity();
connect();
