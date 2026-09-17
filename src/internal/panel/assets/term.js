/* vpsmgr browser terminal.
 *
 * A small VT100/xterm emulator plus the WebSocket session for /terminal. It is
 * hand-written and dependency-free on purpose: the panel ships as one binary,
 * has no build step, and its CSP allows no third-party scripts.
 *
 * Terminal output is arbitrary bytes from a shell, so nothing from the stream
 * is ever interpreted as markup — every character goes through escapeText or
 * textContent.
 */
(function () {
  "use strict";

  var PALETTE = [
    "#1c1c1c", "#d75f5f", "#5faf5f", "#d7af5f", "#5f87d7", "#af5faf", "#5fafaf", "#c6c6c6",
    "#6c6c6c", "#ff8787", "#87d787", "#ffd787", "#87afff", "#ff87ff", "#87ffff", "#ffffff"
  ];

  var F_BOLD = 1, F_DIM = 2, F_ITALIC = 4, F_UNDER = 8, F_BLINK = 16,
      F_REV = 32, F_HIDDEN = 64, F_STRIKE = 128;

  function escapeText(s) {
    return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  }

  function color256(n) {
    if (n < 16) return PALETTE[n];
    if (n < 232) {
      n -= 16;
      var f = function (v) { return v === 0 ? 0 : 55 + v * 40; };
      return "rgb(" + f(Math.floor(n / 36)) + "," + f(Math.floor((n % 36) / 6)) + "," + f(n % 6) + ")";
    }
    var v = 8 + (n - 232) * 10;
    return "rgb(" + v + "," + v + "," + v + ")";
  }

  function isWide(cp) {
    return (cp >= 0x1100 && (
      cp <= 0x115f || cp === 0x2329 || cp === 0x232a ||
      (cp >= 0x2e80 && cp <= 0xa4cf && cp !== 0x303f) ||
      (cp >= 0xac00 && cp <= 0xd7a3) || (cp >= 0xf900 && cp <= 0xfaff) ||
      (cp >= 0xfe30 && cp <= 0xfe6f) || (cp >= 0xff00 && cp <= 0xff60) ||
      (cp >= 0xffe0 && cp <= 0xffe6) || (cp >= 0x20000 && cp <= 0x3fffd)));
  }

  function blankCell() { return { ch: " ", fg: null, bg: null, fl: 0, cont: false }; }

  function blankLine(cols) {
    var cells = new Array(cols);
    for (var i = 0; i < cols; i++) cells[i] = blankCell();
    return { cells: cells };
  }

  function styleOf(c) {
    var fg = c.fg, bg = c.bg, f = c.fl;
    if (f & F_REV) { var t = fg; fg = bg === null ? "#0f172a" : bg; bg = t === null ? "#c6c6c6" : t; }
    if (f & F_HIDDEN) fg = bg === null ? "#0f172a" : bg;
    if (f & F_DIM) fg = fg === null ? "#6c6c6c" : fg;
    var s = "";
    if (fg) s += "color:" + fg + ";";
    if (bg) s += "background:" + bg + ";";
    if (f & F_BOLD) s += "font-weight:700;";
    if (f & F_ITALIC) s += "font-style:italic;";
    if (f & F_UNDER) s += "text-decoration:underline;";
    if (f & F_STRIKE) s += "text-decoration:line-through;";
    return s;
  }

  // ---- emulator ---------------------------------------------------------

  function Term(mount, opts) {
    this.mount = mount;
    this.opts = opts || {};
    this.cols = 80;
    this.rows = 24;
    this.sb = [];
    this.maxSb = 2000;
    this.vy = 0;
    this.cx = 0;
    this.cy = 0;
    this.attr = { fg: null, bg: null, fl: 0 };
    this.saved = null;
    this.scrollTop = 0;
    this.scrollBot = 23;
    this.modes = { wrap: true, cursor: true, appCursor: false, bracketed: false, insert: false, alt: false };
    this.altSaved = null;
    this.state = "ground";
    this.buf = "";
    this.params = [];
    this.priv = false;
    this.osc = "";
    this.dirty = new Set();
    this.needFull = true;
    this.lastCursorRow = -1;
    this.decoder = new TextDecoder("utf-8");
    this.disposed = false;
    this.exited = false;
    this.screen = [];
    for (var i = 0; i < this.rows; i++) this.screen.push(blankLine(this.cols));
    this._buildDom();
    this._measure();
    this._onResize();
  }

  Term.prototype._buildDom = function () {
    var self = this;
    this.mount.classList.add("term");
    this.rowsEl = document.createElement("div");
    this.rowsEl.className = "term-rows";
    this.mount.appendChild(this.rowsEl);
    this.rowEls = [];
    for (var i = 0; i < 24; i++) {
      var d = document.createElement("div");
      d.className = "term-row";
      this.rowsEl.appendChild(d);
      this.rowEls.push(d);
    }
    // A hidden textarea is what receives IME composition and any text the
    // browser inserts without key events (mobile keyboards, autocomplete).
    // Key events we handle ourselves are preventDefault'ed, so nothing lands
    // here for ordinary typing and the two paths never double up.
    this.input = document.createElement("textarea");
    this.input.className = "term-input";
    this.input.setAttribute("autocapitalize", "off");
    this.input.setAttribute("autocorrect", "off");
    this.input.setAttribute("autocomplete", "off");
    this.input.setAttribute("spellcheck", "false");
    this.input.setAttribute("aria-label", "terminal input");
    this.mount.appendChild(this.input);
    this.composing = false;
    this.input.addEventListener("input", function () {
      if (self.composing) return;
      var v = self.input.value;
      if (v) { self.input.value = ""; self._send(v); }
    });
    this.input.addEventListener("compositionstart", function () { self.composing = true; });
    this.input.addEventListener("compositionend", function () {
      self.composing = false;
      var v = self.input.value;
      if (v) { self.input.value = ""; self._send(v); }
    });
    this.mount.addEventListener("keydown", function (e) { self._onKey(e); });
    this.mount.addEventListener("paste", function (e) { self._onPaste(e); });
    this.mount.addEventListener("wheel", function (e) { self._onWheel(e); }, { passive: false });
    // A click must leave focus on the hidden input, never on the mount itself:
    // the mount is focusable so it can host the cursor, and the browser's own
    // focus handling runs after mousedown, hence the deferred call.
    this.mount.addEventListener("mousedown", function () {
      setTimeout(function () { self.input.focus(); }, 0);
    });
    this.mount.addEventListener("focus", function () { self.input.focus(); });
    this.mount.addEventListener("focus", function () { self.needFull = true; self._scheduleRender(); });
    this.mount.addEventListener("blur", function () { self.needFull = true; self._scheduleRender(); });
    this._ro = new ResizeObserver(function () { self._onResize(); });
    this._ro.observe(this.mount);
  };

  Term.prototype._measure = function () {
    var probe = document.createElement("span");
    probe.className = "term-probe";
    probe.textContent = new Array(101).join("W");
    this.mount.appendChild(probe);
    var r = probe.getBoundingClientRect();
    this.cellW = r.width > 0 ? r.width / 100 : 8;
    this.cellH = r.height > 0 ? r.height : 17;
    this.mount.removeChild(probe);
  };

  Term.prototype._onResize = function () {
    var st = getComputedStyle(this.mount);
    var padX = parseFloat(st.paddingLeft) + parseFloat(st.paddingRight);
    var padY = parseFloat(st.paddingTop) + parseFloat(st.paddingBottom);
    var cols = Math.max(2, Math.floor((this.mount.clientWidth - padX) / this.cellW));
    var rows = Math.max(2, Math.floor((this.mount.clientHeight - padY) / this.cellH));
    if (cols === this.cols && rows === this.rows) return;
    this.resize(cols, rows);
    if (this.opts.onResize) this.opts.onResize(cols, rows);
  };

  Term.prototype.resize = function (cols, rows) {
    var all = this.sb.concat(this.screen);
    this.cols = cols;
    this.rows = rows;
    // Existing lines are truncated or padded rather than re-wrapped: good
    // enough for a resize, and it never loses the line being worked on.
    for (var i = 0; i < all.length; i++) {
      var line = all[i];
      if (line.cells.length > cols) line.cells.length = cols;
      while (line.cells.length < cols) line.cells.push(blankCell());
    }
    while (all.length < rows) all.push(blankLine(cols));
    this.screen = all.slice(all.length - rows);
    this.sb = all.slice(0, all.length - rows);
    if (this.sb.length > this.maxSb) this.sb = this.sb.slice(this.sb.length - this.maxSb);

    while (this.rowEls.length < rows) {
      var d = document.createElement("div");
      d.className = "term-row";
      this.rowsEl.appendChild(d);
      this.rowEls.push(d);
    }
    for (var j = 0; j < this.rowEls.length; j++) {
      this.rowEls[j].style.display = j < rows ? "" : "none";
    }
    this.scrollTop = 0;
    this.scrollBot = rows - 1;
    if (this.cy >= rows) this.cy = rows - 1;
    if (this.cx >= cols) this.cx = cols - 1;
    this.needFull = true;
    this._scheduleRender();
  };

  Term.prototype.line = function (y) { return this.screen[y]; };

  Term.prototype._scrollUp = function () {
    this.sb.push(this.screen[this.scrollTop]);
    if (this.sb.length > this.maxSb) this.sb.shift();
    this.screen.splice(this.scrollTop, 1);
    this.screen.push(blankLine(this.cols));
    this.needFull = true;
  };

  Term.prototype._scrollDown = function () {
    this.screen.splice(this.scrollBot, 1);
    this.screen.splice(this.scrollTop, 0, blankLine(this.cols));
    this.needFull = true;
  };

  Term.prototype._lf = function () {
    if (this.cy === this.scrollBot) this._scrollUp();
    else if (this.cy < this.rows - 1) this.cy++;
  };

  // ---- output -----------------------------------------------------------

  Term.prototype.write = function (bytes) {
    var s = this.decoder.decode(bytes, { stream: true });
    if (!s) return;
    this._parse(s);
    this._scheduleRender();
  };

  Term.prototype._parse = function (s) {
    for (var i = 0; i < s.length; i++) {
      var ch = s[i], code = s.charCodeAt(i);
      switch (this.state) {
        case "ground":
          if (code === 0x1b) { this.state = "esc"; break; }
          if (code === 0x0d) { this.cx = 0; break; }
          if (code === 0x0a || code === 0x0b || code === 0x0c) { this._lf(); break; }
          if (code === 0x08) { if (this.cx > 0) this.cx--; break; }
          if (code === 0x09) { this.cx = Math.min(this.cols - 1, (this.cx + 8) & ~7); break; }
          if (code < 0x20) break; // BEL and the rest have no visual effect
          if (code >= 0xd800 && code <= 0xdbff && i + 1 < s.length) { ch = s.substr(i, 2); i++; }
          this._put(ch, ch.codePointAt(0));
          break;
        case "esc":
          if (ch === "[") { this.state = "csi"; this.params = []; this.priv = false; this.buf = ""; }
          else if (ch === "]") { this.state = "osc"; this.osc = ""; }
          else if (ch === "7") { this.saved = { x: this.cx, y: this.cy, a: Object.assign({}, this.attr) }; this.state = "ground"; }
          else if (ch === "8") {
            if (this.saved) { this.cx = this.saved.x; this.cy = this.saved.y; this.attr = Object.assign({}, this.saved.a); }
            this.state = "ground";
          }
          else if (ch === "D") { this._lf(); this.state = "ground"; }
          else if (ch === "M") { if (this.cy === this.scrollTop) this._scrollDown(); else if (this.cy > 0) this.cy--; this.state = "ground"; }
          else if (ch === "E") { this.cx = 0; this._lf(); this.state = "ground"; }
          else if (ch === "c") { this._reset(); this.state = "ground"; }
          else if (ch === "(" || ch === ")" || ch === "*" || ch === "+") { this.state = "charset"; }
          else if (ch === "P" || ch === "_" || ch === "^" || ch === "X") { this.state = "dcs"; }
          else this.state = "ground";
          break;
        case "charset":
          this.state = "ground";
          break;
        case "dcs":
          if (code === 0x07) this.state = "ground";
          else if (code === 0x1b) this.state = "dcsEsc";
          break;
        case "dcsEsc":
          this.state = ch === "\\" ? "ground" : "dcs";
          break;
        case "osc":
          if (code === 0x07) { this._osc(); this.state = "ground"; }
          else if (code === 0x1b) { this._osc(); this.state = "oscEsc"; }
          else this.osc += ch;
          break;
        case "oscEsc":
          this.state = ch === "\\" ? "ground" : "osc";
          break;
        case "csi":
          if (code >= 0x40 && code <= 0x7e) { this._csi(ch); this.state = "ground"; }
          else if (code === 0x3f) this.priv = true;
          else if (code >= 0x30 && code <= 0x39) this.buf += ch;
          else if (code === 0x3b) { this.params.push(parseInt(this.buf, 10)); this.buf = ""; }
          break;
      }
    }
  };

  Term.prototype._osc = function () {
    var m = /^(\d+);?(.*)$/.exec(this.osc);
    if (!m) return;
    var n = parseInt(m[1], 10);
    // 777 is the session's own end-of-session marker; only the title is used.
    if ((n === 0 || n === 2) && this.opts.onTitle) this.opts.onTitle(m[2]);
  };

  Term.prototype._params = function () {
    var p = this.params.slice();
    if (this.buf !== "") p.push(parseInt(this.buf, 10));
    if (p.length === 0) p.push(0);
    for (var i = 0; i < p.length; i++) if (isNaN(p[i])) p[i] = 0;
    return p;
  };

  Term.prototype._csi = function (final) {
    var p = this._params();
    var n = function (i, d) { return p[i] === 0 ? d : p[i]; };
    var i, c, k;
    switch (final) {
      case "A": this.cy = Math.max(this.scrollTop, this.cy - n(0, 1)); break;
      case "B": this.cy = Math.min(this.scrollBot, this.cy + n(0, 1)); break;
      case "C": this.cx = Math.min(this.cols - 1, this.cx + n(0, 1)); break;
      case "D": this.cx = Math.max(0, this.cx - n(0, 1)); break;
      case "E": this.cy = Math.min(this.rows - 1, this.cy + n(0, 1)); this.cx = 0; break;
      case "F": this.cy = Math.max(0, this.cy - n(0, 1)); this.cx = 0; break;
      case "G": this.cx = Math.min(this.cols - 1, n(0, 1) - 1); break;
      case "d": this.cy = Math.min(this.rows - 1, n(0, 1) - 1); break;
      case "H": case "f":
        this.cy = Math.min(this.rows - 1, n(0, 1) - 1);
        this.cx = Math.min(this.cols - 1, n(1, 1) - 1);
        break;
      case "J":
        if (p[0] === 0) { this._eraseLine(this.cy, this.cx, this.cols - 1); for (i = this.cy + 1; i < this.rows; i++) this._eraseLine(i, 0, this.cols - 1); }
        else if (p[0] === 1) { this._eraseLine(this.cy, 0, this.cx); for (i = 0; i < this.cy; i++) this._eraseLine(i, 0, this.cols - 1); }
        else if (p[0] === 2 || p[0] === 3) { for (i = 0; i < this.rows; i++) this._eraseLine(i, 0, this.cols - 1); }
        break;
      case "K":
        if (p[0] === 0) this._eraseLine(this.cy, this.cx, this.cols - 1);
        else if (p[0] === 1) this._eraseLine(this.cy, 0, this.cx);
        else if (p[0] === 2) this._eraseLine(this.cy, 0, this.cols - 1);
        break;
      case "L":
        for (i = 0; i < n(0, 1); i++) this.screen.splice(this.scrollBot, 0, blankLine(this.cols));
        this.screen.length = this.rows;
        this.needFull = true;
        break;
      case "M":
        for (i = 0; i < n(0, 1); i++) { this.screen.splice(this.cy, 1); this.screen.splice(this.scrollBot, 0, blankLine(this.cols)); }
        this.needFull = true;
        break;
      case "P":
        c = this.line(this.cy);
        for (i = 0; i < n(0, 1); i++) { c.cells.splice(this.cx, 1); c.cells.push(blankCell()); }
        this.dirty.add(this.cy);
        break;
      case "@":
        c = this.line(this.cy);
        for (i = 0; i < n(0, 1); i++) { c.cells.splice(this.cx, 0, blankCell()); c.cells.length = this.cols; }
        this.dirty.add(this.cy);
        break;
      case "X":
        this._eraseLine(this.cy, this.cx, Math.min(this.cols - 1, this.cx + n(0, 1) - 1));
        break;
      case "S": for (i = 0; i < n(0, 1); i++) this._scrollUp(); break;
      case "T": for (i = 0; i < n(0, 1); i++) this._scrollDown(); break;
      case "r":
        this.scrollTop = Math.max(0, n(0, 1) - 1);
        this.scrollBot = Math.min(this.rows - 1, n(1, this.rows) - 1);
        if (this.scrollBot <= this.scrollTop) { this.scrollTop = 0; this.scrollBot = this.rows - 1; }
        this.cx = 0; this.cy = this.scrollTop;
        break;
      case "m": this._sgr(p); break;
      case "h": this._mode(p, true); break;
      case "l": this._mode(p, false); break;
      case "n":
        if (p[0] === 6 && this.opts.onReply) this.opts.onReply("\x1b[" + (this.cy + 1) + ";" + (this.cx + 1) + "R");
        break;
      case "c":
        if (this.opts.onReply) this.opts.onReply("\x1b[?1;2c");
        break;
    }
    this._scheduleRender();
  };

  Term.prototype._eraseLine = function (y, from, to) {
    var line = this.line(y);
    if (!line) return;
    for (var i = Math.max(0, from); i <= to && i < this.cols; i++) {
      line.cells[i] = blankCell();
      // Erasing must not leave the trailing half of a wide character behind.
      if (i > 0 && line.cells[i - 1].ch) {
        var prev = line.cells[i - 1];
        if (!prev.cont && isWide(prev.ch.codePointAt(0))) line.cells[i - 1] = blankCell();
      }
    }
    this.dirty.add(y);
  };

  Term.prototype._sgr = function (p) {
    if (p.length === 0) p = [0];
    for (var i = 0; i < p.length; i++) {
      var v = p[i];
      if (v === 0) this.attr = { fg: null, bg: null, fl: 0 };
      else if (v === 1) this.attr.fl |= F_BOLD;
      else if (v === 2) this.attr.fl |= F_DIM;
      else if (v === 3) this.attr.fl |= F_ITALIC;
      else if (v === 4) this.attr.fl |= F_UNDER;
      else if (v === 5) this.attr.fl |= F_BLINK;
      else if (v === 7) this.attr.fl |= F_REV;
      else if (v === 8) this.attr.fl |= F_HIDDEN;
      else if (v === 9) this.attr.fl |= F_STRIKE;
      else if (v === 22) this.attr.fl &= ~(F_BOLD | F_DIM);
      else if (v === 23) this.attr.fl &= ~F_ITALIC;
      else if (v === 24) this.attr.fl &= ~F_UNDER;
      else if (v === 25) this.attr.fl &= ~F_BLINK;
      else if (v === 27) this.attr.fl &= ~F_REV;
      else if (v === 28) this.attr.fl &= ~F_HIDDEN;
      else if (v === 29) this.attr.fl &= ~F_STRIKE;
      else if (v >= 30 && v <= 37) this.attr.fg = PALETTE[v - 30];
      else if (v === 39) this.attr.fg = null;
      else if (v >= 40 && v <= 47) this.attr.bg = PALETTE[v - 40];
      else if (v === 49) this.attr.bg = null;
      else if (v >= 90 && v <= 97) this.attr.fg = PALETTE[v - 82];
      else if (v >= 100 && v <= 107) this.attr.bg = PALETTE[v - 92];
      else if (v === 38 || v === 48) {
        var t = v === 38 ? "fg" : "bg";
        if (p[i + 1] === 5) { this.attr[t] = color256(p[i + 2]); i += 2; }
        else if (p[i + 1] === 2) {
          this.attr[t] = "rgb(" + p[i + 2] + "," + p[i + 3] + "," + p[i + 4] + ")";
          i += 4;
        }
      }
    }
  };

  Term.prototype._mode = function (p, on) {
    for (var i = 0; i < p.length; i++) {
      var v = p[i];
      if (!this.priv) { if (v === 4) this.modes.insert = on; continue; }
      if (v === 25) this.modes.cursor = on;
      else if (v === 7) this.modes.wrap = on;
      else if (v === 1) this.modes.appCursor = on;
      else if (v === 2004) this.modes.bracketed = on;
      else if (v === 47 || v === 1047 || v === 1049) {
        if (on && !this.modes.alt) {
          this.altSaved = { screen: this.screen, cx: this.cx, cy: this.cy };
          this.screen = [];
          for (var k = 0; k < this.rows; k++) this.screen.push(blankLine(this.cols));
          this.cx = 0; this.cy = 0; this.vy = 0;
          this.modes.alt = true;
          this.needFull = true;
        } else if (!on && this.modes.alt && this.altSaved) {
          this.screen = this.altSaved.screen;
          this.cx = this.altSaved.cx;
          this.cy = this.altSaved.cy;
          this.modes.alt = false;
          this.needFull = true;
        }
      }
    }
  };

  Term.prototype._put = function (ch, cp) {
    var wide = isWide(cp);
    if (this.cx >= this.cols) {
      if (!this.modes.wrap) this.cx = this.cols - 1;
      else { this.cx = 0; this._lf(); }
    }
    if (wide && this.cx === this.cols - 1) { this.cx = 0; this._lf(); }
    var line = this.line(this.cy);
    if (this.modes.insert) { line.cells.splice(this.cx, 0, blankCell()); line.cells.length = this.cols; }
    line.cells[this.cx] = { ch: ch, fg: this.attr.fg, bg: this.attr.bg, fl: this.attr.fl, cont: false };
    this.cx++;
    if (wide && this.cx < this.cols) {
      line.cells[this.cx] = { ch: "", fg: this.attr.fg, bg: this.attr.bg, fl: this.attr.fl, cont: true };
      this.cx++;
    }
    this.dirty.add(this.cy);
  };

  Term.prototype._reset = function () {
    this.attr = { fg: null, bg: null, fl: 0 };
    this.modes.alt = false;
    this.screen = [];
    for (var i = 0; i < this.rows; i++) this.screen.push(blankLine(this.cols));
    this.cx = 0; this.cy = 0; this.vy = 0;
    this.scrollTop = 0; this.scrollBot = this.rows - 1;
    this.needFull = true;
  };

  // ---- rendering --------------------------------------------------------

  Term.prototype._scheduleRender = function () {
    if (this._raf || this.disposed) return;
    var self = this;
    this._raf = requestAnimationFrame(function () { self._raf = 0; self._render(); });
  };

  Term.prototype._render = function () {
    if (this.disposed) return;
    var total = this.sb.length + this.screen.length;
    var maxVy = Math.max(0, total - this.rows);
    if (this.vy > maxVy) this.vy = maxVy;
    var start = total - this.rows - this.vy;
    var focused = this.mount.contains(document.activeElement);
    var cursorRow = (this.vy === 0 && !this.modes.alt) ? this.cy : -1;

    for (var y = 0; y < this.rows; y++) {
      var idx = start + y;
      var line = idx < this.sb.length ? this.sb[idx] : this.screen[idx - this.sb.length];
      var live = (y === cursorRow);
      // Repaint only what changed, plus the two rows the cursor can have moved
      // between.
      if (!this.needFull && !this.dirty.has(y) && !live && y !== this.lastCursorRow) continue;
      this.rowEls[y].innerHTML = this._rowHtml(line, live, focused);
    }
    this.dirty.clear();
    this.needFull = false;
    this.lastCursorRow = cursorRow;
  };

  Term.prototype._rowHtml = function (line, live, focused) {
    if (!line) return "";
    var html = "", style = null, buf = "";
    var flush = function () {
      if (!buf) return;
      html += style ? '<span style="' + style + '">' + escapeText(buf) + "</span>" : escapeText(buf);
      buf = "";
    };
    for (var x = 0; x < this.cols; x++) {
      var c = line.cells[x];
      if (!c || c.cont) continue;
      var s = styleOf(c);
      if (x === this.cx && live && focused && this.modes.cursor) s += "background:#38bdf8;color:#0f172a;";
      if (s !== style) { flush(); style = s; }
      buf += c.ch === "" ? " " : c.ch;
    }
    flush();
    if (live && focused && this.modes.cursor && this.cx >= this.cols) {
      html += '<span style="background:#38bdf8;color:#0f172a;"> </span>';
    }
    return html;
  };

  // ---- input ------------------------------------------------------------

  Term.prototype._send = function (s) { if (this.opts.onInput) this.opts.onInput(s); };

  Term.prototype.focus = function () { this.input.focus(); };

  Term.prototype._scrollBy = function (lines) {
    var total = this.sb.length + this.screen.length;
    var max = Math.max(0, total - this.rows);
    this.vy = Math.min(max, Math.max(0, this.vy + lines));
    this.needFull = true;
    this._scheduleRender();
  };

  Term.prototype._onWheel = function (e) {
    if (this.modes.alt) return; // full-screen apps scroll themselves
    var step = e.deltaMode === 1 ? 3 : 1;
    var d = (e.deltaY > 0 ? -1 : 1) * step;
    if (this.vy === 0 && d < 0) return; // already at the bottom
    this._scrollBy(d);
    e.preventDefault();
  };

  Term.prototype._onPaste = function (e) {
    var text = (e.clipboardData || window.clipboardData).getData("text");
    if (!text) return;
    e.preventDefault();
    if (this.modes.bracketed) this._send("\x1b[200~" + text + "\x1b[201~");
    else this._send(text);
  };

  Term.prototype._onKey = function (e) {
    if (this.exited) return;
    if (e.isComposing || e.keyCode === 229) return; // the IME owns this keystroke
    var m = this.modes;
    var send = null;

    if (e.shiftKey && (e.key === "PageUp" || e.key === "PageDown")) {
      this._scrollBy(e.key === "PageUp" ? this.rows : -this.rows);
      e.preventDefault();
      return;
    }
    if (e.ctrlKey && e.shiftKey && (e.key === "C" || e.key === "V")) return; // browser copy/paste
    // Ctrl+C interrupts, unless the user is copying a selection.
    if (e.ctrlKey && !e.altKey && e.key.toLowerCase() === "c" && String(window.getSelection())) return;

    var arrows = { ArrowUp: "A", ArrowDown: "B", ArrowRight: "C", ArrowLeft: "D", Home: "H", End: "F" };
    if (arrows[e.key]) send = m.appCursor ? "\x1bO" + arrows[e.key] : "\x1b[" + arrows[e.key];
    else if (e.key === "Enter") send = "\r";
    else if (e.key === "Backspace") send = "\x7f";
    else if (e.key === "Tab") send = "\t";
    else if (e.key === "Escape") send = "\x1b";
    else if (e.key === "PageUp") send = "\x1b[5~";
    else if (e.key === "PageDown") send = "\x1b[6~";
    else if (e.key === "Insert") send = "\x1b[2~";
    else if (e.key === "Delete") send = "\x1b[3~";
    else if (/^F([1-9]|1[0-2])$/.test(e.key)) {
      var f = parseInt(e.key.slice(1), 10);
      var ss3 = { 1: "P", 2: "Q", 3: "R", 4: "S" };
      send = ss3[f] ? "\x1bO" + ss3[f] : "\x1b[" + [15, 17, 18, 19, 20, 21, 23, 24][f - 5] + "~";
    } else if (e.ctrlKey && !e.altKey && e.key.length === 1) {
      var c = e.key.toUpperCase().charCodeAt(0);
      if (c >= 64 && c < 128) send = String.fromCharCode(c & 0x1f);
      else if (e.key === " ") send = "\x00";
    } else if (e.altKey && e.key.length === 1) {
      send = "\x1b" + e.key;
    } else if (!e.ctrlKey && !e.metaKey && e.key.length === 1) {
      send = e.key;
    }

    if (send === null) {
      // Swallow named keys we do not map so they cannot scroll the page.
      if (e.key.length > 1 && !/^(Shift|Control|Alt|Meta|CapsLock|NumLock|ScrollLock|Dead|Fn|ContextMenu|OS)$/.test(e.key)) {
        e.preventDefault();
      }
      return;
    }
    e.preventDefault();
    if (this.vy !== 0) this._scrollBy(-this.vy);
    this._send(send);
  };

  Term.prototype.dispose = function () {
    this.disposed = true;
    if (this._ro) this._ro.disconnect();
    if (this._raf) cancelAnimationFrame(this._raf);
  };

  // ---- session ----------------------------------------------------------

  // openTerminal wires an emulator to /terminal over a WebSocket. Text frames
  // in both directions carry JSON control messages; terminal output arrives as
  // binary frames so arbitrary bytes survive untouched.
  window.vpsmgrOpenTerminal = function (mount, url, hooks) {
    hooks = hooks || {};
    var status = function (t) { if (hooks.onStatus) hooks.onStatus(t); };
    // Declared before the emulator: it reports its size from its constructor,
    // and that callback must not touch an unset socket.
    var ws = null;
    function control(obj) {
      if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
    }
    function send(data) { control({ t: "i", data: data }); }
    var term = new Term(mount, {
      onTitle: function (t) { if (hooks.onTitle) hooks.onTitle(t); },
      onReply: function (s) { send(s); },
      onInput: function (s) { send(s); },
      onResize: function (c, r) { control({ t: "r", cols: c, rows: r }); }
    });

    var proto = location.protocol === "https:" ? "wss:" : "ws:";
    var q = "?cols=" + term.cols + "&rows=" + term.rows;
    ws = new WebSocket(proto + "//" + location.host + url + q);
    ws.binaryType = "arraybuffer";
    var closed = false;

    ws.onopen = function () {
      // Send the size the emulator settled on; the shell would otherwise start
      // at the server's default until the window is resized.
      control({ t: "r", cols: term.cols, rows: term.rows });
      term.focus();
      status("");
    };
    ws.onmessage = function (e) {
      if (typeof e.data === "string") {
        var msg;
        try { msg = JSON.parse(e.data); } catch (err) { return; }
        if (msg.t === "exit") {
          term.exited = true;
          status("session ended" + (msg.code ? " (exit " + msg.code + ")" : ""));
          if (hooks.onExit) hooks.onExit(msg.code);
        } else if (msg.t === "error") {
          term.exited = true;
          term.write(new TextEncoder().encode("\r\n[ " + msg.error + " ]\r\n"));
          status("error");
          if (hooks.onError) hooks.onError(msg.error);
        }
        return;
      }
      term.write(new Uint8Array(e.data));
    };
    ws.onclose = function () {
      if (closed) return;
      closed = true;
      term.exited = true;
      status("connection closed");
      if (hooks.onClose) hooks.onClose();
    };
    ws.onerror = function () { status("connection error"); };

    return {
      focus: function () { term.focus(); },
      close: function () {
        closed = true;
        try { ws.close(); } catch (e) { /* already gone */ }
        term.dispose();
      }
    };
  };
})();
