// Apply the stored theme before first paint, to avoid a flash of the wrong one. A static file, not
// an inline <script>: an inline script cannot pass the Content-Security-Policy's
// `script-src 'self'` (sec-hardening-2 H-9b). Loaded synchronously from <head> in index.html.
(function () {
  try {
    var t = localStorage.getItem("cerbix-theme");
    if (t === "dark" || t === "light") document.documentElement.setAttribute("data-theme", t);
  } catch (e) {}
})();
