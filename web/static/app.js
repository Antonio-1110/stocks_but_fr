// Sorting and filtering for the company table. No dependencies.
(function () {
  var table = document.getElementById("list");
  if (!table) return;
  var tbody = table.tBodies[0];
  var rows = Array.prototype.slice.call(tbody.rows);
  var q = document.getElementById("q");
  var industry = document.getElementById("industry");
  var count = document.getElementById("count");

  function sortKey(row, col) {
    var cell = row.cells[col];
    var v = cell.getAttribute("data-sort");
    return v === null ? cell.textContent.trim() : v;
  }

  Array.prototype.forEach.call(table.tHead.rows[0].cells, function (th, col) {
    var type = th.getAttribute("data-type");
    if (!type) return;
    th.addEventListener("click", function () {
      // Numbers start high-to-low, text starts A-Z; a second tap flips it.
      var asc = th.getAttribute("aria-sort")
        ? th.getAttribute("aria-sort") === "descending"
        : type === "text" || col === 0;
      Array.prototype.forEach.call(th.parentNode.cells, function (c) { c.removeAttribute("aria-sort"); });
      th.setAttribute("aria-sort", asc ? "ascending" : "descending");
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
    });
  });

  function filter() {
    var words = q.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
    var ind = industry.value;
    var shown = 0;
    rows.forEach(function (r) {
      var text = (r.getAttribute("data-search") || "").toLowerCase();
      var ok = (!ind || r.getAttribute("data-industry") === ind) &&
        words.every(function (w) { return text.indexOf(w) !== -1; });
      r.hidden = !ok;
      if (ok) shown++;
    });
    count.textContent = shown === rows.length ? rows.length : shown + " / " + rows.length;
  }
  q.addEventListener("input", filter);
  industry.addEventListener("change", filter);
})();
