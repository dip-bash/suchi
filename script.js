/* Suchi static site — vanilla JS */
(function () {
  "use strict";

  /* ---------------------------------------------------------------- data */

  var TAB_LABELS = ["Clipboard", "Notes", "Expander"];

  var MOCK_DATA = [
    [
      { text: "sudo pacman -Syu linux-firmware", time: "1m ago", pinned: true },
      { text: "systemctl --user status suchi-daemon.service", time: "2m ago", pinned: false },
      {
        text: "curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/install.sh | bash",
        time: "3m ago",
        pinned: false,
      },
      { text: "Lorem ipsum dolor sit amet, consectetur adipiscing elit.", time: "1h ago", pinned: false },
    ],
    [
      { text: "Server config: evaluate Nginx routing", time: "Today", pinned: true },
      { text: "Meeting notes: adjust Neovim 0.12.2 native completion", time: "Yesterday", pinned: false },
      { text: "Fix shell script piping into Playwright scraper", time: "2d ago", pinned: false },
    ],
    [
      { text: ":mail -> saumyadip@example.com", time: "Active", pinned: false },
      { text: ":gh -> https://github.com/dip-bash", time: "Active", pinned: false },
      { text: ":shrug -> ¯\\_(ツ)_/¯", time: "Active", pinned: true },
    ],
  ];

  var COMMANDS = [
    {
      label: "Install",
      cmd: "curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/install.sh | bash",
    },
    {
      label: "Uninstall",
      cmd: "curl -sSL https://raw.githubusercontent.com/dip-bash/suchi/main/uninstall.sh | bash",
    },
    { label: "Launch TUI", cmd: "suchi" },
    { label: "Check Daemon", cmd: "systemctl --user status suchi-daemon.service" },
  ];

  var KEYBINDINGS = [
    ["Tab", "Cycle workspaces"],
    ["i", "Enter INSERT mode"],
    ["Esc", "Enter NORMAL mode"],
    ["j / k", "Navigate list"],
    ["p", "Pin / Unpin item"],
    ["d", "Delete item"],
    ["y / Enter", "Copy item"],
    ["?", "Toggle this help"],
  ];

  /* ------------------------------------------------------------- helpers */

  function fuzzyMatch(pattern, str) {
    var p = pattern.toLowerCase();
    var s = str.toLowerCase();
    var pi = 0;
    var si = 0;
    while (pi < p.length && si < s.length) {
      if (p[pi] === s[si]) pi++;
      si++;
    }
    return pi === p.length;
  }

  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text).catch(function () {
        return legacyCopy(text);
      });
    }
    return Promise.resolve(legacyCopy(text));
  }

  function legacyCopy(text) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.top = "0";
    ta.style.left = "0";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    var ok = false;
    try {
      ok = document.execCommand("copy");
    } catch (e) {
      ok = false;
    }
    document.body.removeChild(ta);
    return ok;
  }

  /* ---------------------------------------------------------- star count */

  var starEl = document.getElementById("star-count");
  if (starEl) {
    fetch("https://api.github.com/repos/dip-bash/suchi")
      .then(function (r) {
        return r.ok ? r.json() : null;
      })
      .then(function (d) {
        if (d && typeof d.stargazers_count === "number") {
          starEl.textContent = String(d.stargazers_count);
        }
      })
      .catch(function () {});
  }

  /* -------------------------------------------------------- command tabs */

  var cmdTabs = document.getElementById("cmd-tabs");
  var cmdText = document.getElementById("cmd-text");
  var cmdCopy = document.getElementById("cmd-copy");
  var activeCmd = 0;
  var cmdTimer = null;

  function renderCommands() {
    if (!cmdTabs || !cmdText) return;
    cmdTabs.innerHTML = "";
    COMMANDS.forEach(function (c, i) {
      var b = document.createElement("button");
      b.type = "button";
      b.setAttribute("role", "tab");
      b.setAttribute("aria-selected", String(i === activeCmd));
      b.textContent = c.label;
      b.addEventListener("click", function () {
        activeCmd = i;
        renderCommands();
      });
      cmdTabs.appendChild(b);
    });
    cmdText.textContent = COMMANDS[activeCmd].cmd;
  }

  if (cmdCopy) {
    cmdCopy.addEventListener("click", function () {
      copyText(COMMANDS[activeCmd].cmd).then(function () {
        cmdCopy.textContent = "Copied";
        if (cmdTimer) clearTimeout(cmdTimer);
        cmdTimer = setTimeout(function () {
          cmdCopy.textContent = "Copy";
        }, 2000);
      });
    });
  }

  renderCommands();

  /* ------------------------------------------------------- terminal demo */

  var term = document.getElementById("term");
  var termTabs = document.getElementById("term-tabs");
  var listEl = document.getElementById("list");
  var searchEl = document.getElementById("search");
  var modeEl = document.getElementById("mode");
  var helpEl = document.getElementById("help");
  var helpList = document.getElementById("help-list");
  var toastEl = document.getElementById("toast");
  var countEl = document.getElementById("count");

  if (!term || !listEl) return;

  var items = MOCK_DATA.map(function (arr) {
    return arr.map(function (i) {
      return { text: i.text, time: i.time, pinned: i.pinned };
    });
  });
  var tab = 0;
  var mode = "NORMAL";
  var query = "";
  var selected = 0;
  var helpOpen = false;
  var toastTimer = null;

  function visible() {
    var data = items[tab].slice().sort(function (a, b) {
      return a.pinned === b.pinned ? 0 : a.pinned ? -1 : 1;
    });
    if (!query) return data;
    return data.filter(function (i) {
      return fuzzyMatch(query, i.text);
    });
  }

  function flash(msg) {
    if (!toastEl) return;
    toastEl.textContent = msg;
    toastEl.hidden = false;
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      toastEl.hidden = true;
    }, 1400);
  }

  function renderTabs() {
    termTabs.innerHTML = "";
    TAB_LABELS.forEach(function (label, i) {
      var b = document.createElement("button");
      b.type = "button";
      b.setAttribute("role", "tab");
      b.setAttribute("aria-selected", String(i === tab));
      b.textContent = label;
      b.addEventListener("click", function () {
        tab = i;
        selected = 0;
        render();
      });
      termTabs.appendChild(b);
    });
  }

  function renderHelp() {
    if (!helpList) return;
    helpList.innerHTML = "";
    KEYBINDINGS.forEach(function (pair) {
      var li = document.createElement("li");
      var kbd = document.createElement("kbd");
      kbd.textContent = pair[0];
      var span = document.createElement("span");
      span.textContent = pair[1];
      li.appendChild(kbd);
      li.appendChild(span);
      helpList.appendChild(li);
    });
  }

  function render() {
    renderTabs();

    var list = visible();
    if (selected >= list.length) selected = Math.max(0, list.length - 1);

    listEl.innerHTML = "";
    if (list.length === 0) {
      var empty = document.createElement("li");
      empty.className = "empty";
      empty.textContent = "No matches found / Empty";
      listEl.appendChild(empty);
    } else {
      list.forEach(function (item, i) {
        var li = document.createElement("li");
        li.setAttribute("role", "option");
        li.setAttribute("aria-selected", String(i === selected));
        if (i === selected) li.className = "sel";

        var caret = document.createElement("span");
        caret.className = "caret";
        caret.setAttribute("aria-hidden", "true");
        caret.textContent = i === selected ? "→" : "";
        li.appendChild(caret);

        if (item.pinned) {
          var pin = document.createElement("span");
          pin.className = "pin";
          pin.setAttribute("aria-hidden", "true");
          pin.textContent = "●";
          li.appendChild(pin);
        }

        var text = document.createElement("span");
        text.className = "text";
        text.textContent = item.text;
        li.appendChild(text);

        var time = document.createElement("span");
        time.className = "time";
        time.textContent = item.time;
        li.appendChild(time);

        li.addEventListener("click", function () {
          selected = i;
          render();
        });

        listEl.appendChild(li);
      });

      var selNode = listEl.children[selected];
      if (selNode && selNode.scrollIntoView) {
        selNode.scrollIntoView({ block: "nearest" });
      }
    }

    if (countEl) {
      countEl.textContent = list.length + (list.length === 1 ? " item" : " items");
    }
    if (modeEl) {
      modeEl.textContent = mode;
      modeEl.className = mode === "INSERT" ? "mode insert" : "mode";
    }
    if (helpEl) helpEl.hidden = !helpOpen;
  }

  function currentItem() {
    return visible()[selected];
  }

  function doPin() {
    var cur = currentItem();
    if (!cur) return;
    items[tab] = items[tab].map(function (i) {
      if (i.text === cur.text) i.pinned = !i.pinned;
      return i;
    });
    render();
  }

  function doDelete() {
    var cur = currentItem();
    if (!cur) return;
    items[tab] = items[tab].filter(function (i) {
      return i.text !== cur.text;
    });
    render();
  }

  function doCopy() {
    var cur = currentItem();
    if (!cur) return;
    copyText(cur.text).then(function () {
      flash("Copied to clipboard");
    });
  }

  if (searchEl) {
    searchEl.addEventListener("input", function () {
      query = searchEl.value;
      selected = 0;
      render();
    });
    searchEl.addEventListener("focus", function () {
      mode = "INSERT";
      render();
    });
    searchEl.addEventListener("blur", function () {
      mode = "NORMAL";
      render();
    });
  }

  term.addEventListener("keydown", function (e) {
    if (e.key === "Tab") {
      e.preventDefault();
      tab = (tab + 1) % TAB_LABELS.length;
      selected = 0;
      render();
      return;
    }

    if (mode === "INSERT") {
      if (e.key === "Escape") {
        e.preventDefault();
        mode = "NORMAL";
        if (searchEl) searchEl.blur();
        term.focus();
        render();
      }
      return;
    }

    switch (e.key) {
      case "j":
        e.preventDefault();
        selected = Math.min(selected + 1, Math.max(0, visible().length - 1));
        render();
        break;
      case "k":
        e.preventDefault();
        selected = Math.max(0, selected - 1);
        render();
        break;
      case "i":
      case "/":
        e.preventDefault();
        mode = "INSERT";
        render();
        if (searchEl) searchEl.focus();
        break;
      case "p":
        e.preventDefault();
        doPin();
        break;
      case "d":
        e.preventDefault();
        doDelete();
        break;
      case "y":
      case "Enter":
        e.preventDefault();
        doCopy();
        break;
      case "?":
        e.preventDefault();
        helpOpen = !helpOpen;
        render();
        break;
      case "Escape":
        helpOpen = false;
        render();
        break;
    }
  });

  renderHelp();
  render();
})();
