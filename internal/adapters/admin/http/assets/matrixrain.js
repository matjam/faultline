// Faultline admin login background: classic Matrix katakana rain.
//
// Self-contained (no dependencies). Activates only on a canvas with
// id="matrix-rain"; if no such element exists the script is a no-op.
// Respects prefers-reduced-motion (renders one static frame instead
// of animating).
//
// Visual budget: ~30 fps, single 2D canvas, no offscreen surfaces.
// Negligible CPU on a modern laptop; the login page is the only
// surface this runs on.
(function () {
  "use strict";

  var canvas = document.getElementById("matrix-rain");
  if (!canvas || !canvas.getContext) return;
  var ctx = canvas.getContext("2d");

  // Half-width katakana + a sprinkling of digits/punctuation.
  var GLYPHS =
    "ｱｲｳｴｵｶｷｸｹｺｻｼｽｾｿﾀﾁﾂﾃﾄﾅﾆﾇﾈﾉﾊﾋﾌﾍﾎﾏﾐﾑﾒﾓﾔﾕﾖﾗﾘﾙﾚﾛﾜﾝ" +
    "0123456789-+:.{}[]<>/\\|=*";
  var FONT_SIZE = 16;
  var COLS = 0;
  var drops = [];

  function resize() {
    var dpr = window.devicePixelRatio || 1;
    var w = canvas.clientWidth || window.innerWidth;
    var h = canvas.clientHeight || window.innerHeight;
    canvas.width = Math.floor(w * dpr);
    canvas.height = Math.floor(h * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);

    COLS = Math.floor(w / FONT_SIZE);
    drops = new Array(COLS);
    for (var i = 0; i < COLS; i++) {
      // Stagger initial Y so the rain doesn't all start at the top.
      drops[i] = Math.random() * (h / FONT_SIZE);
    }
  }

  function frame() {
    var w = canvas.clientWidth || window.innerWidth;
    var h = canvas.clientHeight || window.innerHeight;

    // Trail-fade overlay. Lower alpha = longer phosphor trail.
    ctx.fillStyle = "rgba(0, 0, 0, 0.06)";
    ctx.fillRect(0, 0, w, h);

    ctx.font = FONT_SIZE + "px 'Share Tech Mono', monospace";

    for (var i = 0; i < COLS; i++) {
      var ch = GLYPHS.charAt(Math.floor(Math.random() * GLYPHS.length));
      var x = i * FONT_SIZE;
      var y = drops[i] * FONT_SIZE;

      // Leading glyph: bright phosphor white-green.
      ctx.fillStyle = "rgba(200, 255, 220, 0.95)";
      ctx.fillText(ch, x, y);

      // Trailing glyphs: dimmer green. Cheap effect: redraw
      // previous-row character at lower opacity so the "stream"
      // looks continuous.
      ctx.fillStyle = "rgba(0, 255, 100, 0.65)";
      if (y - FONT_SIZE > 0) {
        ctx.fillText(
          GLYPHS.charAt(Math.floor(Math.random() * GLYPHS.length)),
          x,
          y - FONT_SIZE
        );
      }

      // Reset to top with random delay once we go off-screen.
      if (y > h && Math.random() > 0.975) {
        drops[i] = 0;
      }
      drops[i]++;
    }
  }

  window.addEventListener("resize", resize, { passive: true });
  resize();

  if (
    window.matchMedia &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches
  ) {
    // Single static frame; no animation loop.
    frame();
    return;
  }

  // Run via setInterval rather than requestAnimationFrame so the rate
  // stays constant regardless of monitor refresh.
  setInterval(frame, 55);
})();
