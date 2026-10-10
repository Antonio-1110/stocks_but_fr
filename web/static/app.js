// Sorting and filtering for the company table. No dependencies.
(function () {
  var table = document.getElementById("list");
  if (!table) return;
  var tbody = table.tBodies[0];
  var rows = Array.prototype.slice.call(tbody.rows);
  var q = document.getElementById("q");
  var industry = document.getElementById("industry");
  var count = document.getElementById("count");
  var showLow = document.getElementById("showlow");
  var headers = table.tHead.rows[0].cells;
  var view = location.hash === "#unpriced" ? "unpriced" : "growth";

  function sortKey(row, col) {
    var cell = row.cells[col];
    var v = cell.getAttribute("data-sort");
    return v === null ? cell.textContent.trim() : v;
  }

  function sortBy(col, asc) {
    var type = headers[col].getAttribute("data-type");
    Array.prototype.forEach.call(headers, function (c) { c.removeAttribute("aria-sort"); });
    headers[col].setAttribute("aria-sort", asc ? "ascending" : "descending");
    rows.sort(function (a, b) {
      var x = sortKey(a, col), y = sortKey(b, col);
      if (type === "num") {
        // Missing values always go last, whichever direction.
        if (x === "" || y === "") return (x === "") - (y === "");
        return asc ? x - y : y - x;
      }
      return asc ? x.localeCompare(y, "zh-Hant") : y.localeCompare(x, "zh-Hant");
    });
    rows.forEach(function (r) { tbody.appendChild(r); });
  }

  Array.prototype.forEach.call(headers, function (th, col) {
    var type = th.getAttribute("data-type");
    if (!type) return;
    th.addEventListener("click", function () {
      // Numbers start high-to-low, text starts A-Z; a second tap flips it.
      var asc = th.getAttribute("aria-sort")
        ? th.getAttribute("aria-sort") === "descending"
        : type === "text" || col === 0;
      sortBy(col, asc);
    });
  });

  // The rank cell shows the current view's rank. The growth view's cell is
  // what the server rendered; remember it so switching back restores it.
  rows.forEach(function (r) {
    var c = r.cells[0];
    r._growth = { html: c.innerHTML, sort: c.getAttribute("data-sort"), title: c.getAttribute("title") };
  });
  function setAttr(el, name, v) {
    if (v === null || v === undefined) el.removeAttribute(name); else el.setAttribute(name, v);
  }
  function setView(v) {
    view = v;
    document.querySelectorAll(".views [data-view]").forEach(function (b) {
      b.setAttribute("aria-selected", b.getAttribute("data-view") === v ? "true" : "false");
    });
    document.querySelectorAll(".hint[data-for]").forEach(function (h) {
      h.hidden = h.getAttribute("data-for") !== v;
    });
    showLow.parentNode.hidden = v === "unpriced";
    rows.forEach(function (r) {
      var c = r.cells[0], g = r._growth;
      if (v === "growth") {
        c.innerHTML = g.html; setAttr(c, "data-sort", g.sort); setAttr(c, "title", g.title);
      } else {
        var n = r.getAttribute("data-unpriced-rank") || "";
        c.textContent = n || "–"; c.setAttribute("data-sort", n);
        setAttr(c, "title", r.getAttribute("data-unpriced-note"));
      }
    });
    history.replaceState(null, "", v === "unpriced" ? "#unpriced" : location.pathname + location.search);
    sortBy(0, true);
    filter();
  }
  document.querySelectorAll(".views [data-view]").forEach(function (b) {
    b.addEventListener("click", function () { setView(b.getAttribute("data-view")); });
  });

  function filter() {
    var words = q.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
    var ind = industry.value;
    var shown = 0;
    rows.forEach(function (r) {
      var text = (r.getAttribute("data-search") || "").toLowerCase();
      var ok = (view === "unpriced" ? r.hasAttribute("data-unpriced-rank")
          : showLow.checked || !r.hasAttribute("data-lowbase")) &&
        (!ind || r.getAttribute("data-industry") === ind) &&
        words.every(function (w) { return text.indexOf(w) !== -1; });
      r.hidden = !ok;
      if (ok) shown++;
    });
    var empty = document.getElementById("unpriced-empty");
    empty.hidden = !(view === "unpriced" && !rows.some(function (r) { return r.hasAttribute("data-unpriced-rank"); }));
    count.textContent = shown === rows.length ? rows.length : shown + " / " + rows.length;
  }
  q.addEventListener("input", filter);
  industry.addEventListener("change", filter);
  showLow.addEventListener("change", filter);
  setView(view); // also hides low-base rows, which start hidden
})();
