// powerwarden UI: re-render the live region when /ui/events reports a change.
(function () {
  "use strict";
  var status = document.getElementById("stream");
  var live = document.getElementById("live");
  if (!window.EventSource || !live) {
    if (status) status.textContent = "live: unsupported";
    return;
  }
  var timer = null;

  function editing() {
    var el = document.activeElement;
    return el && live.contains(el) && (el.tagName === "INPUT" || el.tagName === "SELECT" || el.tagName === "TEXTAREA");
  }

  function refresh() {
    timer = null;
    if (editing()) { schedule(); return; }
    fetch(location.pathname, { headers: { "X-Partial": "1" }, credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (html) { if (html !== null && !editing()) live.innerHTML = html; })
      .catch(function () {});
  }

  function schedule() {
    if (!timer) timer = setTimeout(refresh, 300);
  }

  // The reverse-proxy path prefix, if any, comes from <body data-base>.
  var ui = (document.body.getAttribute("data-base") || "") + "/ui";
  var es = new EventSource(ui + "/events");
  ["demand", "worker", "host", "decision", "claim", "resync"].forEach(function (name) {
    es.addEventListener(name, schedule);
  });
  es.onopen = function () { status.textContent = "live"; status.className = "ok"; };
  es.onerror = function () { status.textContent = "live: reconnecting"; status.className = "bad"; };
  // Countdowns (dwell, grace, cooldown, leases) move without events.
  setInterval(schedule, 15000);
})();
