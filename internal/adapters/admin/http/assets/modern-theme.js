(function () {
  const key = "faultline-admin-theme";

  function preferredTheme() {
    const stored = localStorage.getItem(key);
    if (stored === "light" || stored === "dark") {
      return stored;
    }
    return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
  }

  function applyTheme(theme) {
    document.documentElement.dataset.theme = theme;
    for (const label of document.querySelectorAll("[data-theme-label]")) {
      label.textContent = theme === "dark" ? "dark" : "light";
    }
  }

  applyTheme(preferredTheme());

  document.addEventListener("DOMContentLoaded", function () {
    for (const button of document.querySelectorAll("[data-theme-toggle]")) {
      button.addEventListener("click", function () {
        const next = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
        localStorage.setItem(key, next);
        applyTheme(next);
      });
    }
  });
})();
