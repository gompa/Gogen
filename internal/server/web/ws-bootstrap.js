// Early chat-socket bootstrap.
//
// index.html loads this module BEFORE app.js. Module scripts execute in
// document order, so this one (a few lines, no imports) runs as soon as the
// document has been parsed — while the browser is still fetching app.js's
// static import graph (~25 modules, ~940 KB). The WebSocket handshake and that
// graph download then overlap instead of running back to back: previously the
// page painted the static shell and only called `new WebSocket(...)` from the
// LAST line of app.js, so on a cold cache (or a phone over Wi-Fi) the app sat
// visibly "loaded but not connected" until the whole graph had been fetched,
// parsed and executed.
//
// app.js's connect() ADOPTS this socket instead of opening a second one:
//   - frames received before adoption are buffered here and replayed by
//     connect() in arrival order;
//   - nothing is sent from this module — the outbound protocol (list_sessions,
//     session_attach, …) belongs to app.js and needs its module state;
//   - a socket that errored or closed before adoption is discarded and
//     connect() opens a fresh one, exactly as before.
//
// Opening early is safe because the server's attach sequence is unsolicited:
// session_state/history/config are pushed by the server as soon as the socket
// upgrades, and the frames simply wait in the buffer.
//
// A host without WebSocket (or a refused constructor) leaves the global unset,
// so app.js behaves exactly as it did before — tests and non-browser embeds
// are unaffected.
(() => {
    if (typeof WebSocket !== 'function') return;
    // Ordering guard: index.html loads this module BEFORE app.js, and connect()
    // marks __gogenWSOwned when it runs. If an embed injects the scripts the
    // other way around, app.js has already opened its own socket — opening a
    // second one here would leave an attached server connection nobody adopts.
    if (window.__gogenWSOwned || window.__gogenEarlyWS) return;
    let sock;
    try {
        const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        sock = new WebSocket(`${proto}//${window.location.host}/ws`);
    } catch (err) {
        return; // app.js's connect() is the fallback
    }
    const early = {
        sock,
        closed: false,
        adopted: false,
        buffered: [], // raw payloads received before adoption, in arrival order
    };
    sock.onerror = () => { early.closed = true; };
    sock.onclose = () => { early.closed = true; };
    sock.onmessage = (event) => {
        // After adoption app.js owns onmessage; the guard is belt-and-braces
        // so a frame can never be swallowed into a buffer nobody drains.
        if (!early.adopted) early.buffered.push(event.data);
    };
    window.__gogenEarlyWS = early;
})();
