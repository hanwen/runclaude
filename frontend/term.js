"use strict";

// Shared terminal-pane factory used by both the docked panel (app.js) and the
// pop-out window (popout.js). A pane owns one xterm bound to a host element and
// one WebSocket that auto-reconnects (shells persist host-side, so a reconnect
// replays scrollback and continues).
//
// Output frames are binary PTY bytes. A writable pane wires xterm input back as
// binary (stdin) and a JSON text message for resize. A read-only pane (the
// Presenter) instead honors the server's {"type":"size"} messages and mirrors
// the controller's grid exactly — a viewer can't pick its own size without
// desyncing from the source PTY.
(function () {
  const enc = new TextEncoder();

  function wsURL(path) {
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    return proto + "//" + location.host + path + location.search;
  }

  // makeTerminalPane(hostEl, {path, writable, onSize, onPresenting}).
  //   onSize(cols, rows, term)  fires when a read-only pane receives a size
  //     update (used by the pop-out to resize its window to the shared terminal).
  //   onPresenting(on)          fires on a writable pane when this shell becomes
  //     (or stops being) the one on the shared Presenter view.
  window.makeTerminalPane = function (hostEl, opts) {
    const path = opts.path;
    const writable = !!opts.writable;
    const onSize = opts.onSize;
    const onPresenting = opts.onPresenting;

    const term = new Terminal({
      cursorBlink: writable,
      disableStdin: !writable,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
      fontSize: 13,
      theme: { background: "#0f1115", foreground: "#e6e8ee" },
      scrollback: 5000,
    });
    const fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(hostEl);

    let ws = null, closed = false, retry = null;
    const sendResize = () => {
      if (writable && ws && ws.readyState === 1) {
        ws.send(JSON.stringify({ resize: { cols: term.cols, rows: term.rows } }));
      }
    };
    if (writable) {
      term.onData((d) => { if (ws && ws.readyState === 1) ws.send(enc.encode(d)); });
      term.onResize(sendResize);
    }

    function connect() {
      ws = new WebSocket(wsURL(path));
      ws.binaryType = "arraybuffer";
      ws.onopen = () => sendResize();
      ws.onmessage = (e) => {
        if (typeof e.data === "string") {
          let m; try { m = JSON.parse(e.data); } catch { return; }
          if (m.type === "exit") {
            term.write("\r\n\x1b[90m[shell exited]\x1b[0m\r\n");
          } else if (m.type === "size" && !writable && m.cols && m.rows) {
            term.resize(m.cols, m.rows);
            if (onSize) onSize(m.cols, m.rows, term);
          } else if (m.type === "presenting" && onPresenting) {
            onPresenting(!!m.on);
          }
          return;
        }
        term.write(new Uint8Array(e.data));
      };
      ws.onclose = () => {
        if (closed) return;
        retry = setTimeout(connect, 1000); // shell persists; reconnect & replay
      };
      ws.onerror = () => { try { ws.close(); } catch {} };
    }

    return {
      term, fit, writable,
      start() { if (!ws) connect(); },
      // Only writable panes fit to their container and push the size upstream;
      // read-only panes are sized by the server's size messages.
      doFit() { if (!writable) return; try { fit.fit(); } catch {} sendResize(); },
      stop() {
        closed = true;
        if (retry) clearTimeout(retry);
        if (ws) try { ws.close(); } catch {}
        try { term.dispose(); } catch {}
      },
    };
  };
})();
