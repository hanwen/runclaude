"use strict";

// Pop-out terminal window. It hosts the read-only Presenter and — for
// write-eligible users — their private My-shell, as tabs of one window. A
// read-only viewer gets only the Presenter tab.
//
// The window is user-resizable. Each tab carries its own window size: switching
// to a tab resizes the window to fit that terminal. The Presenter is a mirror,
// so its size is dictated by the controller's grid; the My-shell fits to the
// window (resize the window to resize your shell). When your shell is the one on
// the shared view, a "presenting" badge lights up.

const TABBAR = 34; // px reserved for the tab bar above the terminal host

const presentingBadge = document.getElementById("presenting");
const presenterTabBtn = document.querySelector('.tab[data-tab="presenter"]');
let mayWrite = false;
let active = "presenter";
const panes = {};        // tab -> pane
const lastSize = {};     // tab -> {w,h} outer window size to restore on switch

// While you hold control, the Presenter tab only mirrors your own My-shell, so
// hide it and keep the editable shell in focus; restore it when you stop
// presenting (the Presenter then follows whoever else takes control).
function handlePresenting(on) {
  presentingBadge.classList.toggle("on", on);
  presenterTabBtn.hidden = on;
  if (on && active === "presenter") showTab("myshell");
}

function chrome() {
  return {
    w: Math.max(0, window.outerWidth - window.innerWidth),
    h: Math.max(0, window.outerHeight - window.innerHeight),
  };
}

// resizeWindowToTerm sizes the window so term's grid fits exactly (plus the tab
// bar and window chrome). Used for tabs whose size is content-driven.
function resizeWindowToTerm(term) {
  requestAnimationFrame(() => {
    const el = term.element;
    if (!el) return;
    const r = el.getBoundingClientRect();
    const c = chrome();
    const w = Math.ceil(r.width) + c.w + 16;
    const h = Math.ceil(r.height) + c.h + TABBAR + 8;
    window.resizeTo(w, h);
    lastSize[active] = { w, h };
  });
}

function showTab(tab) {
  active = tab;
  for (const t of ["presenter", "myshell"]) {
    const host = document.getElementById("host-" + t);
    if (host) host.hidden = t !== tab;
    const btn = document.querySelector('.tab[data-tab="' + t + '"]');
    if (btn) btn.classList.toggle("active", t === tab);
  }
  if (tab === "presenter") {
    // Mirror: snap the window to the controller's grid.
    resizeWindowToTerm(panes.presenter.term);
  } else {
    // My-shell fits the window; restore this tab's last size, then fit.
    const s = lastSize.myshell;
    if (s) window.resizeTo(s.w, s.h);
    requestAnimationFrame(() => panes.myshell.doFit());
  }
}

function wireTabs() {
  for (const btn of document.querySelectorAll(".tab")) {
    btn.onclick = () => { if (!btn.hidden) showTab(btn.dataset.tab); };
  }
  window.addEventListener("resize", () => {
    if (active === "myshell" && panes.myshell) {
      panes.myshell.doFit();
      lastSize.myshell = { w: window.outerWidth, h: window.outerHeight };
    }
  });
  window.addEventListener("beforeunload", () => {
    for (const k in panes) panes[k].stop();
  });
}

async function init() {
  try {
    const me = await fetch("/whoami" + location.search).then((r) => r.json());
    mayWrite = !!me.mayWrite;
  } catch { /* default read-only */ }

  panes.presenter = makeTerminalPane(document.getElementById("host-presenter"), {
    path: "/terminal/shared",
    writable: false,
    onSize: (_c, _r, term) => { if (active === "presenter") resizeWindowToTerm(term); },
  });
  panes.presenter.start();

  if (mayWrite) {
    document.querySelector('.tab[data-tab="myshell"]').hidden = false;
    panes.myshell = makeTerminalPane(document.getElementById("host-myshell"), {
      path: "/terminal",
      writable: true,
      onPresenting: handlePresenting,
    });
    panes.myshell.start();
  }

  wireTabs();
  // Write-eligible users land on their editable shell (the Presenter is just a
  // mirror); read-only viewers only have the Presenter.
  showTab(mayWrite ? "myshell" : "presenter");
}

init();
