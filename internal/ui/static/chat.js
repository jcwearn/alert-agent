// The chat window: a launcher in the bottom-right corner of every page that
// opens a conversation with the agent. A conversation ("thread") is either an
// incident's (one per incident, the same one PR comments join) or general,
// about the cluster. The panel stays on its thread across pages; each message
// carries the page it was sent from, and on an incident page a chip offers
// that incident's thread. Server API: /api/chat/*, live turns over
// /runs/{id}/stream (SSE).
(function () {
	"use strict";
	if (!window.fetch || !document.body) { return; }

	var KEY = "alert-agent.chat";
	var page = location.pathname;
	var m = page.match(/^\/incidents\/(\d+)$/);
	var pageIncident = m ? Number(m[1]) : 0;
	var phone = window.matchMedia("(max-width: 640px)");
	var coarse = window.matchMedia("(pointer: coarse)");

	// open, full, thread (id), pending ({kind, incident_id}: a thread made on
	// the first message), draft. Kept per tab, across page loads.
	var st = { open: false, full: false, thread: 0, pending: null, draft: "" };
	try { Object.assign(st, JSON.parse(sessionStorage.getItem(KEY)) || {}); } catch (e) { /* private mode */ }
	function save() {
		try {
			sessionStorage.setItem(KEY, JSON.stringify({ open: st.open, full: st.full, thread: st.thread, pending: st.pending, draft: st.draft }));
		} catch (e) { /* not kept; fine */ }
	}

	var list = null; // {here, threads}
	var view = null; // {thread, messages, active, pr, can}
	var stream = null;
	var streamRun = 0;
	var unread = false;

	// ---- DOM ----

	function el(tag, attrs, kids) {
		var n = document.createElement(tag);
		for (var k in attrs || {}) {
			if (k === "text") { n.textContent = attrs[k]; }
			else if (k === "html") { n.innerHTML = attrs[k]; } // only server-rendered, sanitised Markdown
			else if (k.slice(0, 2) === "on") { n.addEventListener(k.slice(2), attrs[k]); }
			else if (attrs[k] !== null && attrs[k] !== false) { n.setAttribute(k, attrs[k] === true ? "" : attrs[k]); }
		}
		(kids || []).forEach(function (c) { if (c) { n.append(c); } });
		return n;
	}
	var icon = {
		chat: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 5.5A2.5 2.5 0 0 1 6.5 3h11A2.5 2.5 0 0 1 20 5.5v8a2.5 2.5 0 0 1-2.5 2.5H10l-4.2 3.6c-.5.4-1.3.1-1.3-.6V16A2.5 2.5 0 0 1 4 13.5z"/></svg>',
		expand: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M14 4h6v6M10 20H4v-6M20 4l-7 7M4 20l7-7"/></svg>',
		shrink: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 14h6v6M20 10h-6V4M10 14l-7 7M14 10l7-7"/></svg>',
		close: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>',
		down: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 10l5 5 5-5"/></svg>',
		send: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 19V5M6 11l6-6 6 6"/></svg>',
		plus: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>',
	};

	var launcher = el("button", { type: "button", "class": "cw-launch", "aria-controls": "cw-panel", "aria-expanded": "false",
		"aria-label": "Chat with the agent", title: "Chat with the agent", html: icon.chat + '<span class="cw-dot" hidden></span>' });
	var dot = launcher.querySelector(".cw-dot");

	var titleBtn = el("button", { type: "button", "class": "cw-title", "aria-haspopup": "true", "aria-expanded": "false",
		"aria-controls": "cw-menu", onclick: toggleMenu });
	var sub = el("div", { "class": "cw-sub" });
	var fullBtn = el("button", { type: "button", "class": "cw-icon cw-fullbtn", onclick: function () { setFull(!st.full); } });
	var closeBtn = el("button", { type: "button", "class": "cw-icon", "aria-label": "Close chat", title: "Close (Esc)",
		html: icon.close, onclick: function () { setOpen(false); } });
	var menu = el("div", { id: "cw-menu", "class": "cw-menu", hidden: true });
	var switchBar = el("div", { "class": "cw-switch", hidden: true });
	var log = el("div", { "class": "cw-log", "aria-live": "polite", "aria-busy": "false" });
	var notice = el("div", { "class": "cw-notice", role: "status", hidden: true });
	var ta = el("textarea", { rows: "1", maxlength: "8000", "aria-label": "Message the agent", placeholder: "Ask the agent…" });
	var sendBtn = el("button", { type: "submit", "class": "cw-send", "aria-label": "Send", title: "Send", html: icon.send });
	var hint = el("div", { "class": "cw-hint" });
	var form = el("form", { "class": "cw-compose", onsubmit: function (e) { e.preventDefault(); send(); } },
		[el("div", { "class": "cw-box" }, [ta, sendBtn]), hint]);
	var panel = el("section", { id: "cw-panel", "class": "cw-panel", "aria-label": "Chat with the agent", hidden: true }, [
		el("header", { "class": "cw-head" }, [
			el("div", { "class": "cw-heading" }, [titleBtn, sub]),
			fullBtn, closeBtn,
		]),
		menu,
		switchBar,
		log,
		el("div", { "class": "cw-foot" }, [notice, form]),
	]);
	document.body.append(launcher, panel);

	launcher.addEventListener("click", function () { setOpen(!st.open, true); });
	panel.addEventListener("keydown", onKey);
	document.addEventListener("click", function (e) {
		if (!menu.hidden && !menu.contains(e.target) && !titleBtn.contains(e.target)) { closeMenu(false); }
	});
	ta.value = st.draft || "";
	ta.addEventListener("input", function () { st.draft = ta.value; save(); autosize(); });
	ta.addEventListener("keydown", function (e) {
		if (e.key === "Enter" && !e.shiftKey && !e.isComposing && !coarse.matches) {
			e.preventDefault();
			send();
		}
	});
	phone.addEventListener("change", layout);
	if (window.visualViewport) {
		// A phone's keyboard shrinks the visual viewport, not 100dvh.
		window.visualViewport.addEventListener("resize", function () {
			panel.style.setProperty("--cw-vh", window.visualViewport.height + "px");
			stick();
		});
	}
	if (window.ResizeObserver) {
		var atBottom = true;
		log.addEventListener("scroll", function () { atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 40; });
		new ResizeObserver(function () { if (atBottom) { stick(); } }).observe(log);
	}

	// An incident page's "Chat about this incident" opens its thread.
	document.querySelectorAll("[data-chat-incident]").forEach(function (b) {
		b.hidden = false;
		b.addEventListener("click", function () {
			setOpen(true, true);
			loadList().then(function () { if (list && list.here) { pickHere(); } });
		});
	});

	// ---- state ----

	// animate: opened by the user now, rather than restored on a page load.
	function setOpen(open, animate) {
		st.open = open;
		save();
		if (open) { unread = false; }
		panel.classList.toggle("cw-anim", !!animate);
		layout();
		if (open) {
			if (!list) { loadList().then(choose); } else { render(); }
			setTimeout(function () { ta.focus({ preventScroll: true }); stick(); }, 0);
		} else {
			closeMenu(false);
			launcher.focus({ preventScroll: true });
		}
	}

	function setFull(full) {
		st.full = full;
		save();
		layout();
		ta.focus({ preventScroll: true });
		stick();
	}

	var lockedY = null;
	function layout() {
		var full = st.full || phone.matches;
		panel.hidden = !st.open;
		panel.classList.toggle("cw-full", full);
		launcher.hidden = st.open && full;
		launcher.setAttribute("aria-expanded", String(st.open));
		if (st.open && full) {
			panel.setAttribute("role", "dialog");
			panel.setAttribute("aria-modal", "true");
		} else {
			panel.removeAttribute("role");
			panel.removeAttribute("aria-modal");
		}
		fullBtn.hidden = phone.matches;
		fullBtn.innerHTML = st.full ? icon.shrink : icon.expand;
		fullBtn.setAttribute("aria-label", st.full ? "Exit full screen" : "Full screen");
		fullBtn.title = fullBtn.getAttribute("aria-label");
		// Keep the page behind still. overflow:hidden does not stop iOS
		// scrolling the body, so pin it in place and restore the position.
		var lock = st.open && full;
		if (lock && lockedY === null) {
			lockedY = window.scrollY;
			document.body.style.cssText += ";position:fixed;left:0;right:0;top:" + -lockedY + "px";
		} else if (!lock && lockedY !== null) {
			document.body.style.position = document.body.style.left = document.body.style.right = document.body.style.top = "";
			window.scrollTo(0, lockedY);
			lockedY = null;
		}
		dot.hidden = !(busy() || unread) || st.open;
		launcher.classList.toggle("cw-busy", busy());
	}

	function busy() { return !!(view && view.active); }

	function onKey(e) {
		if (e.key === "Escape") {
			e.preventDefault();
			if (!menu.hidden) { closeMenu(true); }
			else if (st.full && !phone.matches) { setFull(false); }
			else { setOpen(false); }
		} else if (!menu.hidden && (e.key === "ArrowDown" || e.key === "ArrowUp")) {
			var items = Array.prototype.slice.call(menu.querySelectorAll("button"));
			var i = items.indexOf(document.activeElement);
			i = e.key === "ArrowDown" ? (i + 1) % items.length : (i <= 0 ? items.length - 1 : i - 1);
			items[i].focus();
			e.preventDefault();
		}
	}

	// ---- server ----

	function api(method, path, body) {
		var init = { method: method, credentials: "same-origin", headers: { Accept: "application/json" } };
		if (body) {
			init.headers["Content-Type"] = "application/json";
			init.body = JSON.stringify(body);
		}
		return fetch(path, init).then(function (r) {
			return r.json().catch(function () { return {}; }).then(function (j) {
				if (!r.ok) {
					var err = new Error(j.error || "the controller answered " + r.status);
					err.status = r.status;
					throw err;
				}
				return j;
			});
		});
	}

	function loadList() {
		return api("GET", "/api/chat/threads?page=" + encodeURIComponent(page)).then(function (j) {
			list = j;
		}, function (err) { showNotice("Could not load conversations: " + err.message); });
	}

	// choose picks the thread to show when the panel opens: the one it was
	// on, else this incident's, else the latest general one, else a new one.
	function choose() {
		if (st.thread) { return loadThread(st.thread); }
		if (st.pending) { view = null; render(); return; }
		if (list && list.here) { return pickHere(); }
		var g = list && list.threads.filter(function (t) { return t.kind === "general"; })[0];
		if (g) { return pick(g.id); }
		pickNew();
	}

	function pick(id) {
		st.thread = id;
		st.pending = null;
		save();
		return loadThread(id);
	}
	function pickHere() {
		var h = list.here;
		if (h.thread) { return pick(h.thread.id); }
		st.thread = 0;
		st.pending = { kind: "incident", incident_id: h.incident_id };
		save();
		view = null;
		watch(0);
		render();
	}
	function pickNew() {
		st.thread = 0;
		st.pending = { kind: "general" };
		save();
		view = null;
		watch(0);
		render();
		ta.focus({ preventScroll: true });
	}

	function loadThread(id) {
		log.setAttribute("aria-busy", "true");
		return api("GET", "/api/chat/threads/" + id).then(function (j) {
			if (st.thread !== id) { return; }
			view = j;
			log.setAttribute("aria-busy", "false");
			watch(j.active ? j.active.run_id : 0);
			render();
			stick();
		}, function (err) {
			log.setAttribute("aria-busy", "false");
			if (err.status === 404) {
				st.thread = 0;
				save();
				if (list) { choose(); }
				return;
			}
			showNotice("Could not load the conversation: " + err.message);
		});
	}

	// watch follows a turn over SSE: its steps while it runs, then the
	// thread again (with the reply) when it ends.
	function watch(runID) {
		if (runID === streamRun) { return; }
		if (stream) { stream.close(); stream = null; }
		streamRun = runID;
		steps = [];
		if (!runID || !window.EventSource) { return; }
		stream = new EventSource("/runs/" + runID + "/stream");
		stream.addEventListener("item", function (e) {
			var it = JSON.parse(e.data);
			// Steps are shown as plain text; drop Markdown's code ticks.
			var line = it.kind === "text" ? it.body.replace(/`/g, "") : it.kind === "tool" ? it.title : "";
			if (line) {
				steps.push({ kind: it.kind, text: line });
				renderLive();
			}
		});
		stream.addEventListener("status", function (e) {
			if (view && view.active) { view.active.status = JSON.parse(e.data); }
			renderLive();
		});
		stream.addEventListener("done", function () {
			stream.close();
			stream = null;
			streamRun = 0;
			if (!st.open) { unread = true; }
			if (st.thread) { loadThread(st.thread); }
		});
	}
	var steps = [];

	function send() {
		var body = ta.value.trim();
		if (!body || sendBtn.disabled) { return; }
		sendBtn.disabled = true;
		hideNotice();
		var ready = st.thread ? Promise.resolve(st.thread) : api("POST", "/api/chat/threads", st.pending || { kind: "general" }).then(function (t) {
			st.thread = t.id;
			st.pending = null;
			save();
			return t.id;
		});
		ready.then(function (id) {
			return api("POST", "/api/chat/threads/" + id + "/messages", { body: body, page: page }).then(function (res) {
				ta.value = st.draft = "";
				save();
				autosize();
				if (res.capped) { showNotice(res.capped); }
				loadList();
				return loadThread(id);
			});
		}).catch(function (err) {
			showNotice("Not sent: " + err.message);
		}).then(function () {
			sendBtn.disabled = false;
			ta.focus({ preventScroll: true });
		});
	}

	// ---- rendering ----

	function render() {
		var t = view && view.thread;
		titleBtn.innerHTML = "";
		titleBtn.append(el("span", { "class": "cw-title-text", text: t ? t.title : pendingTitle() }), el("span", { "class": "cw-caret", html: icon.down }));
		titleBtn.title = "Switch conversation";
		renderSub();
		renderSwitch();
		renderLog();
		renderHint();
		layout();
		document.dispatchEvent(new CustomEvent("chat:render"));
	}

	function pendingTitle() {
		if (st.pending && st.pending.kind === "incident" && list && list.here) { return list.here.title; }
		return "New conversation";
	}

	function renderSub() {
		sub.innerHTML = "";
		var t = view && view.thread;
		if (!t) {
			sub.append(st.pending && st.pending.kind === "incident" ? "This incident · starts with your first message" : "About the cluster · starts with your first message");
			return;
		}
		if (t.kind === "general") {
			sub.append("About the cluster");
			return;
		}
		sub.append(el("a", { href: "/incidents/" + t.incident_id, text: "Incident #" + t.incident_id }));
		if (t.namespace) { sub.append(" · " + t.namespace); }
		if (view.pr) {
			sub.append(" · ", el("a", { href: view.pr.url, rel: "noreferrer", target: "_blank", text: "PR #" + view.pr.number }),
				" ", el("span", { "class": "status status-" + view.pr.state, text: view.pr.state }));
		}
	}

	// renderSwitch offers this page's incident thread when the panel is on
	// another one.
	function renderSwitch() {
		switchBar.innerHTML = "";
		var h = list && list.here;
		var on = view && view.thread ? view.thread.incident_id : st.pending && st.pending.incident_id;
		if (!h || on === h.incident_id) {
			switchBar.hidden = true;
			return;
		}
		var what = h.merged_into ? "#" + pageIncident + " is merged into #" + h.merged_into + "." : "You are on #" + h.incident_id + ".";
		switchBar.append(el("span", { text: what + " Your messages say so; or" }),
			el("button", { type: "button", "class": "cw-chip-btn", text: "switch to #" + h.incident_id + "'s conversation", onclick: pickHere }));
		switchBar.hidden = false;
	}

	function renderLog() {
		log.innerHTML = "";
		log.append(intro());
		var msgs = view ? view.messages : [];
		if (!msgs.length && !busy()) { log.append(suggestions()); }
		msgs.forEach(function (msg) { log.append(message(msg)); });
		live = null;
		if (busy()) { renderLive(); }
	}

	function intro() {
		var can = view ? view.can : ["read the cluster", "read incidents", "merge and link incidents"];
		if (!view && st.pending && st.pending.kind === "incident") { can = can.concat("work on a fix as a PR"); }
		var text = can.length > 2 ? can.slice(0, -1).join(", ") + ", and " + can[can.length - 1] : can.join(" and ");
		return el("p", { "class": "cw-intro", text: "Here the agent can " + text + ". It merges, links or commits only when you ask." });
	}

	function suggestions() {
		var incident = view ? view.thread.kind === "incident" : st.pending && st.pending.kind === "incident";
		var ideas = incident
			? ["What happened here?", "Is this related to anything else that fired?", "What would you change, and why?"]
			: ["How is the cluster?", "What is firing right now?", "Are any of the open incidents the same problem?"];
		return el("div", { "class": "cw-ideas" }, ideas.map(function (s) {
			return el("button", { type: "button", "class": "cw-chip-btn", text: s, onclick: function () {
				ta.value = st.draft = s;
				save();
				autosize();
				ta.focus();
			} });
		}));
	}

	var authors = { agent: "Agent", ui: "You" };
	var notes = { capped: "not processed: over the turn limit", failed: "the turn failed", queued: "waiting for the agent" };

	function message(msg) {
		var meta = el("div", { "class": "cw-meta" });
		if (msg.source === "github") {
			meta.append(el("span", { "class": "cw-author", text: "@" + msg.author }), " on ",
				msg.url ? el("a", { href: msg.url, rel: "noreferrer", target: "_blank", text: "GitHub" }) : "GitHub");
		} else {
			meta.append(el("span", { "class": "cw-author", text: authors[msg.source] || msg.author }));
		}
		meta.append(" · ", el("time", { datetime: msg.created_at, title: new Date(msg.created_at).toLocaleString(), text: clock(msg.created_at) }));
		if (msg.from) { meta.append(" ", el("span", { "class": "cw-from", title: "Sent from this page", text: msg.from })); }
		if (msg.source === "agent" && msg.run_id) { meta.append(" · ", el("a", { href: "/runs/" + msg.run_id, text: "run #" + msg.run_id })); }
		var kids = [meta];
		if (msg.path) { kids.push(el("div", { "class": "cw-path", text: msg.path + (msg.line ? ":" + msg.line : "") })); }
		kids.push(msg.html ? el("div", { "class": "cw-body md", html: msg.html }) : el("div", { "class": "cw-body cw-text", text: msg.text }));
		// A waiting message needs no note while the turn shows below it.
		if (notes[msg.status] && msg.source !== "agent" && !(msg.status === "queued" && busy())) {
			kids.push(el("div", { "class": "cw-note cw-note-" + msg.status, text: notes[msg.status] }));
		}
		return el("article", { "class": "cw-msg cw-" + msg.source }, kids);
	}

	var live = null;
	function renderLive() {
		if (!busy()) { return; }
		var a = view.active;
		if (!live) {
			live = el("div", { "class": "cw-msg cw-agent cw-live" });
			log.append(live);
		}
		live.innerHTML = "";
		live.append(el("div", { "class": "cw-meta" }, [
			el("span", { "class": "cw-author", text: "Agent" }), " · ",
			el("a", { href: "/runs/" + a.run_id, text: "run #" + a.run_id }),
		]));
		var shown = steps.slice(-6);
		if (steps.length > shown.length) {
			live.append(el("div", { "class": "cw-step cw-more", text: steps.length - shown.length + " earlier steps" }));
		}
		shown.forEach(function (s) {
			live.append(el("div", { "class": "cw-step cw-step-" + s.kind, text: s.kind === "tool" ? "→ " + s.text : s.text }));
		});
		live.append(el("div", { "class": "cw-thinking" }, [
			el("span", { "class": "cw-dots", "aria-hidden": "true" }, [el("i"), el("i"), el("i")]),
			a.status === "queued" ? "Waiting for a runner…" : "Working…",
		]));
		renderHint();
		layout();
	}

	function renderHint() {
		var a = view && view.active;
		hint.textContent = !a ? (coarse.matches ? "" : "Enter to send · Shift+Enter for a new line")
			: a.status === "queued" ? "A turn is waiting to start; messages sent now join it."
				: "The agent is working; messages sent now start the next turn.";
	}

	// ---- the thread switcher ----

	function toggleMenu() { if (menu.hidden) { openMenu(); } else { closeMenu(true); } }

	function openMenu() {
		renderMenu();
		menu.hidden = false;
		titleBtn.setAttribute("aria-expanded", "true");
		var cur = menu.querySelector("[aria-current]") || menu.querySelector("button");
		if (cur) { cur.focus(); }
		loadList().then(function () { if (!menu.hidden) { renderMenu(); } });
	}

	function closeMenu(focus) {
		menu.hidden = true;
		titleBtn.setAttribute("aria-expanded", "false");
		if (focus) { titleBtn.focus(); }
	}

	function renderMenu() {
		menu.innerHTML = "";
		var cur = view && view.thread ? view.thread.id : 0;
		var h = list && list.here;
		function item(title, meta, current, onpick) {
			return el("button", { type: "button", "class": "cw-item", "aria-current": current ? "true" : null, onclick: function () {
				closeMenu(false);
				onpick();
				ta.focus({ preventScroll: true });
			} }, [el("span", { "class": "cw-item-title", text: title }), meta ? el("span", { "class": "cw-item-meta", text: meta }) : null]);
		}
		if (h) {
			menu.append(el("div", { "class": "cw-group", text: "This page" }));
			menu.append(item(h.thread ? h.thread.title : h.title, h.thread ? describe(h.thread) : "No conversation yet",
				h.thread ? h.thread.id === cur : !!(st.pending && st.pending.incident_id === h.incident_id), pickHere));
		}
		var others = list ? list.threads.filter(function (t) { return !h || !h.thread || t.id !== h.thread.id; }) : [];
		if (others.length) {
			menu.append(el("div", { "class": "cw-group", text: "Recent" }));
			others.forEach(function (t) {
				menu.append(item(t.title, describe(t), t.id === cur, function () { pick(t.id); }));
			});
		}
		menu.append(el("button", { type: "button", "class": "cw-item cw-new", onclick: function () { closeMenu(false); pickNew(); } },
			[el("span", { html: icon.plus }), el("span", { text: "New conversation about the cluster" })]));
	}

	function describe(t) {
		var parts = [t.kind === "general" ? "General" : t.namespace || "incident"];
		parts.push(t.messages === 1 ? "1 message" : t.messages + " messages");
		parts.push(ago(t.updated_at));
		return parts.join(" · ");
	}

	// ---- bits ----

	function showNotice(text) {
		notice.innerHTML = "";
		notice.append(el("span", { text: text }), el("button", { type: "button", "class": "cw-icon", "aria-label": "Dismiss",
			html: icon.close, onclick: hideNotice }));
		notice.hidden = false;
	}
	function hideNotice() { notice.hidden = true; }

	function autosize() {
		ta.style.height = "auto";
		var cs = getComputedStyle(ta);
		var max = parseFloat(cs.lineHeight) * 8 + parseFloat(cs.paddingTop) + parseFloat(cs.paddingBottom);
		ta.style.height = Math.min(ta.scrollHeight + 2, max) + "px";
		ta.style.overflowY = ta.scrollHeight > max ? "auto" : "hidden";
	}

	function stick() { log.scrollTop = log.scrollHeight; }

	function clock(iso) {
		var d = new Date(iso);
		var hm = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
		return d.toDateString() === new Date().toDateString() ? hm : d.toLocaleDateString([], { month: "short", day: "numeric" }) + ", " + hm;
	}

	function ago(iso) {
		var s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
		if (s < 60) { return "just now"; }
		if (s < 3600) { return Math.floor(s / 60) + "m ago"; }
		if (s < 86400) { return Math.floor(s / 3600) + "h ago"; }
		return Math.floor(s / 86400) + "d ago";
	}

	// ---- start ----

	layout();
	autosize();
	if (st.open) {
		loadList().then(choose);
	} else if (st.thread) {
		// Closed, but a turn may be running: the launcher shows it.
		loadThread(st.thread);
	}
})();
