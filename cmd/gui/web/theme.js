// Light/dark theme: applied before the page renders (so there's no flash
// of the wrong theme), remembered per browser. Dark is the default.
(function () {
  const KEY = 'swarmdialer-theme';
  let theme = 'dark';
  try { if (localStorage.getItem(KEY) === 'light') theme = 'light'; } catch (e) { /* storage blocked: default */ }
  document.documentElement.dataset.theme = theme;

  function syncButton() {
    const btn = document.getElementById('theme-toggle');
    if (!btn) return;
    const light = document.documentElement.dataset.theme === 'light';
    btn.textContent = light ? '☾' : '☀'; // moon in light mode, sun in dark mode
    btn.title = light ? 'Switch to dark mode' : 'Switch to light mode';
    btn.setAttribute('aria-label', btn.title);
  }

  window.toggleTheme = function () {
    const next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem(KEY, next); } catch (e) { /* not remembered, still switches */ }
    syncButton();
    document.dispatchEvent(new CustomEvent('themechange', {detail: next}));
  };

  document.addEventListener('DOMContentLoaded', syncButton);
})();
