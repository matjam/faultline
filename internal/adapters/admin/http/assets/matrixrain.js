// Faultline admin login background: classic Matrix katakana rain.
//
// Self-contained (no dependencies). Activates only on a canvas with
// id="matrix-rain"; if no such element exists the script is a no-op.
// Animation is driven by requestAnimationFrame with a wall-clock
// throttle so it stays at ~18 fps regardless of monitor refresh.
//
// Reduced-motion preference is honoured by slowing the rain down
// (one row per ~600 ms) rather than freezing it; a single static
// snapshot reads as "broken" more than as "respectful of accessibility".
(function () {
  "use strict";

  // Bind on either DOMContentLoaded (initial load) or immediately
  // when the DOM is already parsed (HTMX swap, repeat init).
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot, { once: true });
  } else {
    boot();
  }

  function boot() {
    var canvas = document.getElementById("matrix-rain");
    if (!canvas || !canvas.getContext) return;
    var ctx = canvas.getContext("2d");
    if (!ctx) return;

    // Half-width katakana + a sprinkling of digits/punctuation.
    var GLYPHS =
      "ｱｲｳｴｵｶｷｸｹｺｻｼｽｾｿﾀﾁﾂﾃﾄﾅﾆﾇﾈﾉﾊﾋﾌﾍﾎﾏﾐﾑﾒﾓﾔﾕﾖﾗﾘﾙﾚﾛﾜﾝ" +
      "0123456789-+:.{}[]<>/\\|=*";
    var FONT_SIZE = 16;
    var COLS = 0;
    var ROWS = 0;
    var drops = [];
    var w = 0;
    var h = 0;

    var reduced =
      window.matchMedia &&
      window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    var FRAME_INTERVAL = reduced ? 600 : 55; // ms between row advances

    function pickGlyph() {
      return GLYPHS.charAt(Math.floor(Math.random() * GLYPHS.length));
    }

    function resize() {
      var dpr = window.devicePixelRatio || 1;
      // Always trust the viewport — the canvas is position:fixed
      // inset:0, but its clientWidth can briefly read 0 before
      // CSS finishes applying.
      w = window.innerWidth || document.documentElement.clientWidth;
      h = window.innerHeight || document.documentElement.clientHeight;
      canvas.width = Math.floor(w * dpr);
      canvas.height = Math.floor(h * dpr);
      canvas.style.width = w + "px";
      canvas.style.height = h + "px";
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);

      COLS = Math.max(1, Math.floor(w / FONT_SIZE));
      ROWS = Math.max(1, Math.floor(h / FONT_SIZE));
      drops = new Array(COLS);
      for (var i = 0; i < COLS; i++) {
        // Stagger initial Y so the rain doesn't all start at the top.
        drops[i] = Math.random() * ROWS;
      }

      // Paint a solid black bg so the trail-fade has something to
      // start from.
      ctx.fillStyle = "#000";
      ctx.fillRect(0, 0, w, h);
    }

    function step() {
      // Trail-fade overlay. Lower alpha = longer phosphor trail.
      ctx.fillStyle = "rgba(0, 0, 0, 0.07)";
      ctx.fillRect(0, 0, w, h);

      ctx.font = FONT_SIZE + "px 'Share Tech Mono', ui-monospace, monospace";

      for (var i = 0; i < COLS; i++) {
        var x = i * FONT_SIZE;
        var y = drops[i] * FONT_SIZE;

        // Trailing glyph: dim phosphor green.
        ctx.fillStyle = "rgba(0, 255, 100, 0.85)";
        ctx.fillText(pickGlyph(), x, y);

        // Bright leading glyph one row above for a streak.
        ctx.fillStyle = "rgba(220, 255, 230, 0.95)";
        if (y > FONT_SIZE) {
          ctx.fillText(pickGlyph(), x, y - FONT_SIZE);
        }

        // Reset to top with random delay once we go off-screen.
        if (y > h && Math.random() > 0.975) {
          drops[i] = 0;
        }
        drops[i]++;
      }
    }

    var lastStep = 0;
    function loop(now) {
      if (now - lastStep >= FRAME_INTERVAL) {
        step();
        lastStep = now;
      }
      window.requestAnimationFrame(loop);
    }

    window.addEventListener("resize", resize, { passive: true });
    resize();
    // Kick off — one synchronous step so the canvas has visible
    // glyphs even if the rAF loop is slow to start.
    step();
    window.requestAnimationFrame(loop);
  }
})();
