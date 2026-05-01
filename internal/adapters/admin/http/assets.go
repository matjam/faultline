package adminhttp

import "embed"

// staticFS holds the vendored frontend assets shipped with Faultline.
//
// Pinned versions:
//   - htmx.min.js               2.0.9   (https://htmx.org)
//   - tailwind.js               4.x     (@tailwindcss/browser; in-browser JIT)
//   - daisyui.css               5.5.19  (https://daisyui.com)
//   - daisyui-themes.css        5.5.19
//
// Tailwind in the browser was chosen over a build-step CLI because the
// admin UI traffic is low (a single operator on loopback) and avoiding
// a Node-flavored toolchain was an explicit requirement. The runtime
// JIT cost is a one-time ~50ms parse on first paint.
//
// htmx.LICENSE travels with the asset; daisyUI and Tailwind are
// served under their respective MIT licenses recorded in the upstream
// distribution files.
//
//go:embed assets
var staticFS embed.FS

// templateFS holds the html/template files used by the admin UI.
//
//go:embed templates
var templateFS embed.FS
