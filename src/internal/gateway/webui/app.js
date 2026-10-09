/* OpenModel chat + wallet registration app.
 * Zero dependencies on purpose: the only privileged interface is the user's own
 * wallet (EIP-1193 window.ethereum); everything else is plain fetch against the
 * same-origin gateway. Chain reads go to the public chain RPC from /v1/webconfig
 * so balances render without switching the wallet's active network.
 *
 * Precomputed 4-byte selectors (keccak of the canonical signature — constants so
 * the page needs no keccak implementation):
 *   depositFIL()                     0x090d9362
 *   getUserBalance(address,address)  0x6805d6ad
 */
"use strict";

const $ = (id) => document.getElementById(id);
const LS = { key: "om_api_key", wallet: "om_wallet", msgs: "om_messages", session: "om_session", refunds: "om_refunds", walletRdns: "om_wallet_rdns", imgSize: "om_img_size" };

/* API keys are wallet-scoped: a key bills the wallet it was registered under,
 * so adopting one wallet's key while ANOTHER wallet is connected silently
 * charges (and 402s) against the wrong account. Keys are stored per address;
 * the bare legacy slot serves the no-wallet flow ("I have a key") and is
 * migrated to the remembered wallet's slot once on boot. */
function keyFor(addr) {
  return addr ? LS.key + "::" + addr.toLowerCase() : LS.key;
}
function initialKey() {
  const saved = localStorage.getItem(LS.wallet);
  if (saved) {
    const scoped = keyFor(saved);
    const legacy = localStorage.getItem(LS.key);
    if (legacy && !localStorage.getItem(scoped)) {
      // One-time migration: the pre-scoping key belonged to the remembered wallet.
      localStorage.setItem(scoped, legacy);
      localStorage.removeItem(LS.key);
    }
    return localStorage.getItem(scoped) || "";
  }
  return localStorage.getItem(LS.key) || "";
}

/* Contract call constants (precomputed keccak selectors/topics — no crypto lib). */
const SEL = {
  deposit: "0x090d9362",        // depositFIL()
  balance: "0x6805d6ad",        // getUserBalance(address,address)
  requestRefund: "0x99652de7",  // requestRefund(address,uint256)
  approve: "0x095ea7b3",        // ERC20 approve(address,uint256)
  depositToken: "0x338b5dea",   // depositToken(address,uint256)
  claimRefund: "0x5b7baf64",    // claimRefund(uint256)
  getRefund: "0x0abfbb75",      // getRefundRequest(uint256)
};
const REFUND_TOPIC = "0x32b2ff4edc00ceb4bf4534bfef2cc81b2a936c72565da74f77a464902451101f"; // RefundRequested

const state = {
  cfg: null,            // /v1/webconfig payload
  wallet: null,         // connected EVM address (checksummed by the wallet)
  provider: null,       // the SELECTED EIP-1193 provider (EIP-6963 pick or window.ethereum)
  providerInfo: null,   // {name, rdns, icon} of the selected wallet, when announced
  apiKey: "", // set to initialKey() at boot (wallet-scoped; see keyFor)
  sessionId: localStorage.getItem(LS.session) || crypto.randomUUID(),
  messages: JSON.parse(localStorage.getItem(LS.msgs) || "[]"), // [{role, content, meta?}]
  streaming: false,
  abort: null,          // AbortController of the in-flight stream (Stop button)
  thinking: false,      // per-request opt-in: ask the model to reason before answering
  filPrice: 0,          // USD per FIL (live when /v1/me refreshes it)
  prices: null,         // {out, inp, cache} USD per TOKEN for the default model
  catalog: null,        // model id → {input, output, cache, available, modality, context_window, max_output}
  attach: [],           // images staged for the NEXT message: [{dataUrl,w,h,name,bytes,tokens}]
  attachSrc: [],        // decoded originals, 1:1 with `attach`; kept so a size change re-encodes them
};

state.apiKey = initialKey();

/* ---------------- boot ---------------- */

async function boot() {
  try {
    state.cfg = await (await fetch("/v1/webconfig")).json();
  } catch {
    setStatus("Cannot reach the gateway — is it up?");
    return;
  }
  const c = state.cfg;
  if (c.chain && c.chain.chainName) {
    $("chain-badge").textContent = c.chain.chainName.replace("Filecoin ", "");
    $("chain-badge").hidden = false;
  }
  // The verification entry points are the per-reply receipt links (each carries a
  // request_id). We only hint at them here — a bare /receipt-proof/ link without an
  // id just returns a usage error and confuses users.
  if (c.public_query_url) {
    $("verify-hint").hidden = false;
  }
  if (c.fil_price_usd) state.filPrice = parseFloat(c.fil_price_usd);
  if (c.prices_usd) {
    state.prices = {
      out: parseFloat(c.prices_usd.default_output_per_mtok || "0") / 1e6,
      inp: parseFloat(c.prices_usd.default_input_per_mtok || "0") / 1e6,
      cache: parseFloat(c.prices_usd.default_cache_read_per_mtok || "0") / 1e6,
    };
  }
  buildCatalog(c);
  buildTokenSelects(c);
  updateDepositEstimate();
  initWalletDiscovery();
  wireEvents();
  $("img-size").value = localStorage.getItem(LS.imgSize) || "1024";
  onModelChanged();          // sets the attach button's visibility for the first model
  renderAttachments();
  renderKey();
  renderMessages();
  if (state.apiKey) { loadModels(); refreshMe(); }
  // Silent reconnect waits a beat so EIP-6963 announcements (dispatched above)
  // have arrived and the REMEMBERED wallet — not the injection-war winner — is
  // the one asked for accounts.
  const savedWallet = localStorage.getItem(LS.wallet);
  if (savedWallet) setTimeout(() => reconnectSilently(savedWallet), 450);
}

/* ---------------- wallet discovery + selection (EIP-6963) ---------------- */
/* With several wallet extensions installed, bare window.ethereum belongs to
 * whichever injected last — not a user choice. EIP-6963 fixes this: the page
 * broadcasts a request, every wallet announces {info, provider}, and the USER
 * picks. Wallets that predate the spec still work via the window.ethereum
 * fallback (zero announcements). */

const announcedWallets = new Map(); // rdns → {info, provider}

function initWalletDiscovery() {
  window.addEventListener("eip6963:announceProvider", (e) => {
    const d = e.detail;
    if (!d || !d.info || !d.provider) return;
    announcedWallets.set(d.info.rdns || d.info.uuid, d);
    updateConnectAvailability();
  });
  window.dispatchEvent(new Event("eip6963:requestProvider"));
  setTimeout(updateConnectAvailability, 400); // legacy-only environments settle here
}

function updateConnectAvailability() {
  const any = announcedWallets.size > 0 || !!window.ethereum;
  $("btn-connect").disabled = !any;
  $("no-wallet-hint").hidden = any;
}

/* The active provider for every wallet call. Never window.ethereum directly once
 * a selection exists. */
function provider() {
  return state.provider || window.ethereum;
}

function setProvider(p, info) {
  const prev = state.provider;
  if (prev && prev.removeListener) {
    prev.removeListener("accountsChanged", onAccountsChanged);
    prev.removeListener("chainChanged", onChainChanged);
  }
  state.provider = p;
  state.providerInfo = info || null;
  // Follow the wallet: account switches re-bind the page identity, network
  // switches just get corrected on the next transaction (ensureChain).
  p.on?.("accountsChanged", onAccountsChanged);
  p.on?.("chainChanged", onChainChanged);
}

function onAccountsChanged(accs) {
  if (accs && accs.length) onWallet(accs[0]);
  else location.reload(); // disconnected — reset to the clean state
}

function onChainChanged() {
  refreshBalance();
}

function showWalletPicker() {
  const box = $("wallet-picker");
  box.innerHTML = "";
  for (const { info, provider: p } of announcedWallets.values()) {
    const btn = document.createElement("button");
    btn.className = "wallet-option";
    // Per EIP-6963 the icon MUST be a data: URI; refuse anything else.
    const icon = info.icon && info.icon.startsWith("data:image/")
      ? `<img src="${info.icon}" alt="" class="wallet-icon">` : "";
    btn.innerHTML = `${icon}<span>${esc(info.name || info.rdns || "wallet")}</span>`;
    btn.onclick = () => useWallet(p, info);
    box.appendChild(btn);
  }
  box.hidden = false;
}

async function useWallet(p, info) {
  if (!p) return;
  try {
    // Already-authorized sites answer eth_accounts silently; only fall back to
    // the permission prompt when the wallet reports no connected account. This
    // avoids re-prompting on every visit AND most "request already pending"
    // collisions (MetaMask queues an unanswered permission popup indefinitely
    // and -32002s every new request until the user deals with it).
    let accounts = [];
    try { accounts = (await p.request({ method: "eth_accounts" })) || []; } catch { /* fall through to prompt */ }
    if (!accounts.length) accounts = await p.request({ method: "eth_requestAccounts" });
    setProvider(p, info);
    if (info && info.rdns) localStorage.setItem(LS.walletRdns, info.rdns);
    $("wallet-picker").hidden = true;
    onWallet(accounts[0]);
  } catch (e) {
    if (e && (e.code === -32002 || /already pending/i.test(e.message || ""))) {
      setStatus("This wallet already has a connection request waiting — open the wallet's extension icon in your browser toolbar and approve (or dismiss) it there, then try again.");
    } else {
      setStatus("Wallet connection rejected: " + errMsg(e));
    }
  }
}

function wireEvents() {
  $("btn-connect").onclick = connectWallet;
  $("btn-register").onclick = register;
  $("btn-deposit").onclick = deposit;
  $("btn-usekey").onclick = () => {
    const k = $("manual-key").value.trim();
    if (k) setKey(k);
  };
  $("btn-copykey").onclick = () => navigator.clipboard.writeText(state.apiKey);
  $("btn-forgetkey").onclick = () => {
    if (confirm("Forget the API key stored in this browser? Make sure you have a copy — there is no recovery yet.")) setKey("");
  };
  $("btn-newchat").onclick = () => {
    state.messages = [];
    state.sessionId = crypto.randomUUID();
    persist();
    renderMessages();
  };
  $("model-select").onchange = () => {
    localStorage.setItem(LS.model, $("model-select").value);
    $("model-select").dataset.want = $("model-select").value;
    updateModelPrice();
    onModelChanged();
  };
  $("btn-loadkeys").onclick = loadKeyList;
  $("btn-createkey").onclick = createKey;
  $("btn-copynewkey").onclick = () => navigator.clipboard.writeText($("new-key-full").textContent);
  $("btn-usenewkey").onclick = () => {
    setKey($("new-key-full").textContent);
    setStatus("New key is now active in this browser.");
  };
  // One button, both truths: the gateway view (/v1/me) AND the on-chain
  // contract balance. They are different data paths, and refreshing only one
  // made the wallet card look stale next to a fresh account card.
  $("btn-refresh-me").onclick = () => { refreshMe(); refreshBalance(); };
  $("btn-goto-register").onclick = () => {
    $("key-card").scrollIntoView({ behavior: "smooth", block: "center" });
    $("btn-register").classList.add("pulse");
    setTimeout(() => $("btn-register").classList.remove("pulse"), 2400);
  };
  $("btn-goto-paste").onclick = () => {
    $("key-card").scrollIntoView({ behavior: "smooth", block: "center" });
    const d = $("key-none").querySelector("details");
    if (d) d.open = true;
    $("manual-key").focus();
  };
  $("btn-switchwallet").onclick = () => {
    // Back to the picker: forget the remembered choice, show the disconnected
    // card with the wallet list open. Existing key/session state is untouched.
    localStorage.removeItem(LS.walletRdns);
    $("wallet-connected").hidden = true;
    $("wallet-disconnected").hidden = false;
    showWalletPicker();
  };
  $("btn-copyf4").onclick = () => navigator.clipboard.writeText($("f4-addr").textContent);
  $("btn-copysnippet").onclick = () => navigator.clipboard.writeText($("api-snippet").textContent);
  $("btn-refund").onclick = requestRefund;
  $("btn-stop").onclick = () => { if (state.abort) state.abort.abort(); };
  $("thinking-toggle").onchange = (e) => { state.thinking = e.target.checked; };
  $("deposit-amount").addEventListener("input", updateDepositEstimate);
  $("btn-send").onclick = send;

  /* ---- image attachments ---- */
  $("btn-attach").onclick = () => {
    if (!modelIsVision()) {
      setStatus("The selected model does not read images — pick a vision model first.");
      return;
    }
    $("file-input").click();
  };
  $("file-input").onchange = (e) => { addFiles(e.target.files); e.target.value = ""; };
  $("img-size").onchange = async () => {
    localStorage.setItem(LS.imgSize, $("img-size").value);
    try {
      await reencodeAttachments();   // applies to images already attached, not just the next one
    } catch (e) {
      setStatus(errMsg(e));
    }
  };
  // Drag & drop anywhere in the composer, plus paste-into-textarea for screenshots.
  const comp = $("composer");
  comp.addEventListener("dragover", (e) => {
    if (!Array.from(e.dataTransfer.types || []).includes("Files")) return;
    e.preventDefault();
    comp.classList.add("droptarget");
  });
  comp.addEventListener("dragleave", () => comp.classList.remove("droptarget"));
  comp.addEventListener("drop", (e) => {
    comp.classList.remove("droptarget");
    if (!e.dataTransfer.files || !e.dataTransfer.files.length) return;
    e.preventDefault();
    addFiles(e.dataTransfer.files);
  });
  document.addEventListener("paste", (e) => {
    const items = e.clipboardData && e.clipboardData.items;
    if (!items) return;
    const files = [];
    for (const it of items) {
      if (it.kind === "file" && it.type.startsWith("image/")) {
        const f = it.getAsFile();
        if (f) files.push(f);
      }
    }
    if (files.length) { e.preventDefault(); addFiles(files); }
  });
  $("input").addEventListener("input", renderBudget);   // typed text counts against the budget
  $("input").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  });
}

/* ---------------- wallet ---------------- */

async function connectWallet() {
  const remembered = localStorage.getItem(LS.walletRdns);
  if (remembered && announcedWallets.has(remembered)) {
    const w = announcedWallets.get(remembered);
    return useWallet(w.provider, w.info);
  }
  if (announcedWallets.size > 1) return showWalletPicker();   // the user decides
  if (announcedWallets.size === 1) {
    const w = announcedWallets.values().next().value;
    return useWallet(w.provider, w.info);
  }
  return useWallet(window.ethereum, null); // pre-6963 wallet
}

async function reconnectSilently(saved) {
  const remembered = localStorage.getItem(LS.walletRdns);
  const entry = remembered ? announcedWallets.get(remembered) : null;
  const p = entry ? entry.provider : window.ethereum;
  if (!p) return;
  try {
    const accounts = await p.request({ method: "eth_accounts" });
    if (accounts.some((a) => a.toLowerCase() === saved.toLowerCase())) {
      setProvider(p, entry ? entry.info : null);
      onWallet(saved);
    }
  } catch { /* stay disconnected */ }
}

function onWallet(addr) {
  const prev = state.wallet;
  state.wallet = addr;
  localStorage.setItem(LS.wallet, addr);
  // Swap in THIS wallet's key. Without this, switching accounts kept the old
  // wallet's key active and every request billed (and 402'd) the old wallet.
  const scoped = localStorage.getItem(keyFor(addr)) || "";
  if (prev && prev.toLowerCase() !== addr.toLowerCase() && scoped !== state.apiKey) {
    state.apiKey = scoped;
    renderKey();               // no key stored for this wallet → guides to create/paste one
    if (scoped) loadModels();
  } else if (!prev && scoped && scoped !== state.apiKey) {
    // First connect in this tab: prefer the connected wallet's own key over
    // whatever the legacy/no-wallet slot held.
    state.apiKey = scoped;
    renderKey();
    loadModels();
  }
  $("wallet-disconnected").hidden = true;
  $("wallet-connected").hidden = false;
  $("wallet-addr").textContent = short(addr);
  $("wallet-addr").title = addr;
  $("btn-register").disabled = false;
  $("key-manage").hidden = false; // key management is wallet-signature-driven
  $("withdraw-card").hidden = false;
  $("wallet-via").textContent = state.providerInfo ? "via " + state.providerInfo.name : "";
  $("btn-switchwallet").hidden = announcedWallets.size < 2;
  refreshBalance();
  loadF4Addr();
  if (state.cfg.faucet_url) {
    $("faucet-link").href = state.cfg.faucet_url;
    $("faucet-link").hidden = false;
  }
  renderRefunds();
}

/* The f410 form is what exchanges/Lotus wallets need as a send target; derivation
 * lives server-side (it needs blake2b) behind /v1/f4addr. */
async function loadF4Addr() {
  try {
    const r = await (await fetch("/v1/f4addr?wallet=" + state.wallet)).json();
    $("f4-addr").textContent = r.f4 || "unavailable";
  } catch {
    $("f4-addr").textContent = "unavailable";
  }
}

/* Make the wallet's active network match the gateway's settlement chain before a
 * transaction. 4902 = unknown chain → offer to add it from the webconfig preset. */
async function ensureChain() {
  const want = "0x" + Number(state.cfg.chain_id).toString(16);
  const cur = await provider().request({ method: "eth_chainId" });
  if (cur === want) return;
  try {
    await provider().request({ method: "wallet_switchEthereumChain", params: [{ chainId: want }] });
  } catch (e) {
    if (e.code !== 4902 || !state.cfg.chain) throw e;
    await provider().request({
      method: "wallet_addEthereumChain",
      params: [{ chainId: want, ...state.cfg.chain }],
    });
  }
}

/* ---------------- registration ---------------- */

async function register() {
  if (!state.wallet) return;
  setStatus("Fetching the registration message…");
  try {
    const m = await (await fetch("/v1/register/message?wallet=" + state.wallet)).json();
    if (!m.message) throw new Error(m.error ? m.error.message || JSON.stringify(m.error) : "bad response");
    setStatus("Sign the message in your wallet…");
    const sig = await provider().request({
      method: "personal_sign",
      params: ["0x" + utf8ToHex(m.message), state.wallet],
    });
    const res = await fetch("/v1/register", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ wallet: m.wallet, issued_at: m.issued_at, signature: sig }),
    });
    const body = await res.json();
    if (res.ok) {
      setKey(body.api_key);
      setStatus("Registered. Your API key is stored in this browser — keep a copy.");
    } else if (res.status === 409 && /wallet already registered/.test(errBody(body))) {
      setStatus("This wallet is already registered. Use “Load (sign)” to see your keys, or create a new one — a key's full value is only shown at creation.");
      $("key-manage").hidden = false;
    } else {
      setStatus("Registration failed: " + errBody(body));
    }
  } catch (e) {
    setStatus("Registration failed: " + errMsg(e));
  }
}

/* ---------------- key management (signed actions) ---------------- */

/* One wallet signature per management action: fetch the server-composed message,
 * sign, post. The wallet is the account; API keys deliberately cannot manage keys. */
async function keysAction(action, extra = {}) {
  const qs = new URLSearchParams({ wallet: state.wallet, action, ...extra });
  const m = await (await fetch("/v1/keys/message?" + qs)).json();
  if (!m.message) throw new Error(errBody(m));
  setStatus("Sign the " + action + " request in your wallet…");
  const sig = await provider().request({
    method: "personal_sign",
    params: ["0x" + utf8ToHex(m.message), state.wallet],
  });
  const res = await fetch("/v1/keys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      wallet: m.wallet, action: m.action, issued_at: m.issued_at,
      name: m.name || undefined, key_id: m.key_id || undefined, signature: sig,
    }),
  });
  const body = await res.json();
  if (!res.ok) throw new Error(errBody(body));
  setStatus("");
  return body;
}

async function loadKeyList() {
  if (!state.wallet) return;
  try {
    const body = await keysAction("list");
    renderKeyList(body.keys || []);
  } catch (e) {
    setStatus("Could not load keys: " + errMsg(e));
  }
}

function renderKeyList(keys) {
  const box = $("key-list");
  box.innerHTML = "";
  if (!keys.length) {
    box.innerHTML = '<p class="hint">No keys yet — create one below.</p>';
    return;
  }
  const activeMasked = state.apiKey ? keyDisplayJS(state.apiKey) : "";
  for (const k of keys) {
    const row = document.createElement("div");
    row.className = "keyrow";
    const active = k.display && k.display === activeMasked ? " · active here" : "";
    row.innerHTML = `<div class="keyrow-info"><b>${esc(k.name)}</b>${k.static ? " <span class='hint'>(operator)</span>" : ""}
      <div class="hint mono">${esc(k.display || "")}${esc(active)}</div></div>`;
    if (!k.static) {
      const del = document.createElement("button");
      del.className = "danger small";
      del.textContent = "Delete";
      del.onclick = () => deleteKey(k);
      row.appendChild(del);
    }
    box.appendChild(row);
  }
}

async function deleteKey(k) {
  if (!confirm(`Delete key "${k.name}" (${k.display})? Anything using it stops working immediately.`)) return;
  try {
    await keysAction("delete", { key_id: k.id });
    if (state.apiKey && keyDisplayJS(state.apiKey) === k.display) setKey(""); // revoked the active one
    await loadKeyList();
    setStatus("Key deleted.");
  } catch (e) {
    setStatus("Delete failed: " + errMsg(e));
  }
}

async function createKey() {
  if (!state.wallet) return;
  try {
    const name = $("new-key-name").value.trim();
    const body = await keysAction("create", name ? { name } : {});
    $("new-key-full").textContent = body.api_key;
    $("new-key-reveal").hidden = false;
    $("new-key-name").value = "";
    if (!state.apiKey) setKey(body.api_key); // first key: adopt it right away
    await loadKeyList();
  } catch (e) {
    setStatus("Create failed: " + errMsg(e));
  }
}

/* mirror of the server's keyDisplay() so the list can mark the locally-active key */
function keyDisplayJS(key) {
  return key.length <= 14 ? key : key.slice(0, 10) + "…" + key.slice(-4);
}

/* "/models/Qwen--Qwen2.5-3B-Instruct" → "Qwen2.5-3B-Instruct" (display only; the
 * option VALUE keeps the exact id the gateway routes on). */
function friendlyModelName(id) {
  const base = id.split("/").filter(Boolean).pop() || id;
  const parts = base.split("--");
  return parts[parts.length - 1] || base;
}

function setKey(k) {
  state.apiKey = k;
  const slot = keyFor(state.wallet);
  if (k) localStorage.setItem(slot, k);
  else localStorage.removeItem(slot);
  renderKey();
  if (k) loadModels();
}

function renderKey() {
  const have = !!state.apiKey;
  $("key-none").hidden = have;
  $("key-have").hidden = !have;
  // The account card stays visible either way: hiding it taught users nothing.
  // Without a key it explains WHY the numbers need one and offers the two ways
  // to get going; with one it shows the numbers.
  $("me-guide").hidden = have;
  $("me-available").parentElement.hidden = !have;
  $("me-pending").parentElement.hidden = !have;
  if (have) {
    $("key-masked").textContent = state.apiKey.slice(0, 10) + "…" + state.apiKey.slice(-4);
    updateApiSnippet();
    refreshMe();
  }
  $("btn-send").disabled = !have || state.streaming;
}

/* The copy-paste API example. Uses the CURRENTLY selected model (never "default",
 * which the gateway rejects); tracks the dropdown so a copied snippet always works. */
function updateApiSnippet() {
  const el = $("api-snippet");
  if (!el || !state.apiKey) return;
  const model = $("model-select").value ||
    Object.keys(state.catalog || {}).find((id) => id !== "default") ||
    "MODEL_ID";
  el.textContent =
    `curl ${location.origin}/v1/chat/completions \\\n` +
    `  -H "Authorization: Bearer ${state.apiKey}" \\\n` +
    `  -H "Content-Type: application/json" \\\n` +
    `  -d '{"model":${JSON.stringify(model)},"messages":[{"role":"user","content":"hello"}]}'`;
}

/* ---------------- account self-view ---------------- */

async function refreshMe() {
  if (!state.apiKey) return;
  try {
    const res = await fetch("/v1/me", { headers: { Authorization: "Bearer " + state.apiKey } });
    if (!res.ok) {
      // A silent return here left the dashes unexplained — the single most
      // confusing state this UI shipped. Say what is wrong, in place.
      $("me-available").textContent = res.status === 401 ? "key not accepted" : "error " + res.status;
      $("me-pending").textContent = res.status === 401
        ? "this key was rejected by the gateway — re-register or paste a valid one"
        : "gateway error — try Refresh again";
      return;
    }
    const me = await res.json();
    const b = me.balance || {};
    if (b.fil_price_usd) state.filPrice = parseFloat(b.fil_price_usd);
    $("me-available").textContent = b.available_usd != null ? "$" + trimNum(b.available_usd) : "—";
    $("me-pending").textContent = b.pending_usd != null ? "$" + trimNum(b.pending_usd) : "—";
    if (b.tokens && $("me-pending").parentElement) {
      const detail = Object.entries(b.tokens).map(([sym, v]) => `${trimNum(v)} ${sym}`).join(" · ");
      $("me-pending").title = detail ? "on-chain: " + detail : "";
    }
    // The deposited contract balance is shown once, on the wallet card ("In
    // contract"). /v1/me still returns chain_fil for API consumers; the UI does
    // not repeat it here (it read the same getUserBalance the wallet card does).
    // Back-fill receipt chips with the gateway-billed amount (same source as
    // this panel, so the two can never disagree again).
    for (const u of me.recent_usage || []) {
      if (!u.request_id || !u.cost_usd) continue;
      document.querySelectorAll(`.chip-cost[data-rid="${CSS.escape(u.request_id)}"]`).forEach((el) => {
        el.textContent = ` ≈ $${parseFloat(u.cost_usd).toFixed(6)}`;
      });
    }
    const box = $("me-usage");
    const rows = me.recent_usage || [];
    box.innerHTML = rows.length ? "" : "no requests since gateway start";
    for (const u of rows.slice(0, 10)) {
      const div = document.createElement("div");
      const cost = u.cost_usd ? " · $" + trimNum(u.cost_usd) : "";
      div.textContent = `${u.ts.slice(11, 19)} ${u.status} ${u.prompt_tokens}+${u.completion_tokens}tok${cost}`;
      box.appendChild(div);
    }
    updateDepositEstimate();
    return b;
  } catch { /* panel keeps stale values */ }
}

/* "0.5 FIL ≈ $0.38 ≈ ~0.6M output tokens" under the deposit box. */
function updateDepositEstimate() {
  const el = $("deposit-estimate");
  const amt = parseFloat($("deposit-amount").value) || 1;
  if (!state.filPrice || !state.prices || !state.prices.out) {
    el.textContent = "";
    return;
  }
  const usd = amt * state.filPrice;
  const mtok = usd / (state.prices.out * 1e6);
  el.textContent = `${amt} ${nativeSymbol()} ≈ $${usd.toFixed(2)} ≈ ~${mtok.toFixed(1)}M output tokens`;
}

function trimNum(s) {
  const f = parseFloat(s);
  return f >= 1 ? f.toFixed(2) : f.toFixed(6).replace(/0+$/, "").replace(/\.$/, "");
}

/* Currency helpers: the token list comes from /v1/webconfig (gateway
 * supported_tokens). FIL is the native zero address; ERC-20 deposits are the
 * two-step approve → depositToken flow. */
function tokenList() {
  return (state.cfg && state.cfg.tokens) || [{ symbol: "FIL", address: "0x0000000000000000000000000000000000000000", decimals: 18 }];
}
function selectedToken(selId) {
  const addr = ($(selId) && $(selId).value) || "0x0000000000000000000000000000000000000000";
  return tokenList().find((t) => t.address.toLowerCase() === addr.toLowerCase()) || tokenList()[0];
}
function buildTokenSelects(_cfg) {
  for (const selId of ["deposit-token", "refund-token"]) {
    const el = $(selId);
    if (!el) continue;
    el.innerHTML = "";
    for (const t of tokenList()) {
      const o = document.createElement("option");
      o.value = t.address;
      o.textContent = t.symbol;
      el.appendChild(o);
    }
  }
}
function toUnits(amount, decimals) {
  // 6-decimal input precision, then scale to the token's decimals.
  return BigInt(Math.round(amount * 1e6)) * 10n ** BigInt(decimals - 6);
}
async function waitReceipt(tx, label) {
  for (let i = 0; i < 60; i++) {
    const r = await rpcCall("eth_getTransactionReceipt", [tx]).catch(() => null);
    if (r) {
      if (r.status !== "0x1") throw new Error(label + " transaction reverted");
      return r;
    }
    await sleep(5000);
  }
  throw new Error(label + " confirmation timed out");
}

/* ---------------- balance + deposit ---------------- */

async function rpcCall(method, params) {
  const url = state.cfg.chain && state.cfg.chain.rpcUrls && state.cfg.chain.rpcUrls[0];
  if (!url) throw new Error("no chain RPC configured");
  const res = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ jsonrpc: "2.0", id: 1, method, params }),
  });
  const body = await res.json();
  if (body.error) throw new Error(body.error.message);
  return body.result;
}

async function refreshBalance() {
  if (!state.wallet || !state.cfg.contract_address) return;
  try {
    // One getUserBalance(wallet, token) per supported token: the headline line
    // keeps FIL (the native default); other currencies list underneath.
    const extra = [];
    for (const t of tokenList()) {
      const data = "0x6805d6ad" + pad32(state.wallet) + pad32(t.address);
      const out = await rpcCall("eth_call", [{ to: state.cfg.contract_address, data }, "latest"]);
      const human = formatFIL(BigInt(out));
      if (t.symbol === "FIL") {
        $("contract-balance").textContent = human + " " + nativeSymbol();
      } else if (BigInt(out) > 0n) {
        extra.push(`${human} ${t.symbol}`);
      }
    }
    // Same visual weight as the FIL line above: one kv row per extra currency,
    // value bold-right like the native balance (user feedback: no small-print).
    const box = $("contract-balances");
    if (box) box.innerHTML = extra.map((t) => `<div class="kv"><span></span><b>${esc(t)}</b></div>`).join("");
  } catch (e) {
    $("contract-balance").textContent = "read failed";
    $("contract-balance").title = errMsg(e);
  }
}

async function deposit() {
  const amount = parseFloat($("deposit-amount").value);
  if (!state.wallet || !(amount > 0)) return;
  try {
    await ensureChain();
    const tok = selectedToken("deposit-token");
    const units = toUnits(amount, tok.decimals);
    let tx;
    if (tok.symbol === "FIL") {
      setStatus("Confirm the deposit in your wallet…");
      tx = await provider().request({
        method: "eth_sendTransaction",
        params: [{ from: state.wallet, to: state.cfg.contract_address, value: "0x" + units.toString(16), data: SEL.deposit }],
      });
    } else {
      // ERC-20: approve first, then depositToken — two wallet confirmations.
      setStatus(`Step 1/2 — approve ${tok.symbol} in your wallet…`);
      const approveTx = await provider().request({
        method: "eth_sendTransaction",
        params: [{ from: state.wallet, to: tok.address, data: SEL.approve + pad32(state.cfg.contract_address) + pad32("0x" + units.toString(16)) }],
      });
      $("deposit-status").textContent = `approve sent — waiting for confirmation…`;
      await waitReceipt(approveTx, "approve");
      setStatus(`Step 2/2 — confirm the ${tok.symbol} deposit…`);
      tx = await provider().request({
        method: "eth_sendTransaction",
        params: [{ from: state.wallet, to: state.cfg.contract_address, data: SEL.depositToken + pad32(tok.address) + pad32("0x" + units.toString(16)) }],
      });
    }
    $("deposit-status").innerHTML = `deposit sent: ${explorerLink(tx)} — waiting for the gateway to see it…`;
    setStatus("");
    // Readiness = the GATEWAY's view (available_usd), not the chain: its balance
    // cache refreshes every ~15s after the deposit lands, and only then does the
    // 402 gate open. Poll /v1/me when a key exists, the chain otherwise.
    const before = state.apiKey ? (await refreshMe())?.available_usd : null;
    for (let i = 0; i < 24; i++) {
      await sleep(10000);
      if (state.apiKey) {
        const b = await refreshMe();
        if (b && before != null && parseFloat(b.available_usd) > parseFloat(before)) {
          $("deposit-status").innerHTML = `✓ funds visible to the gateway — ready to chat`;
          return;
        }
      } else {
        await refreshBalance();
      }
    }
    $("deposit-status").textContent = "still waiting — check the explorer link above";
  } catch (e) {
    setStatus("Deposit failed: " + errMsg(e));
  }
}

function explorerLink(txHash) {
  const ex = state.cfg.chain && state.cfg.chain.blockExplorerUrls && state.cfg.chain.blockExplorerUrls[0];
  return ex
    ? `<a href="${ex}/message/${txHash}" target="_blank" rel="noopener" class="mono">${esc(short(txHash))}</a>`
    : `<span class="mono">${esc(short(txHash))}</span>`;
}

/* ---------------- withdrawals (requestRefund → timelock → claimRefund) ---------------- */

function savedRefunds() { return JSON.parse(localStorage.getItem(LS.refunds) || "[]"); }
function saveRefunds(list) { localStorage.setItem(LS.refunds, JSON.stringify(list)); }

async function requestRefund() {
  const amount = parseFloat($("refund-amount").value);
  if (!state.wallet || !(amount > 0)) return;
  try {
    await ensureChain();
    setStatus("Confirm the withdrawal request in your wallet…");
    const wei = BigInt(Math.round(amount * 1e6)) * 10n ** 12n;
    const rtok = selectedToken("refund-token");
    const data = SEL.requestRefund + pad32(rtok.address) + pad32("0x" + wei.toString(16));
    const tx = await provider().request({
      method: "eth_sendTransaction",
      params: [{ from: state.wallet, to: state.cfg.contract_address, data }],
    });
    setStatus("Withdrawal request sent — waiting for the receipt…");
    // The request id is emitted in the RefundRequested event; poll the receipt
    // and read topics[1]. Needed later for the claim call.
    for (let i = 0; i < 30; i++) {
      await sleep(10000);
      const rcpt = await rpcCall("eth_getTransactionReceipt", [tx]).catch(() => null);
      if (!rcpt) continue;
      const log = (rcpt.logs || []).find((l) => l.topics && l.topics[0] === REFUND_TOPIC);
      if (!log) { setStatus("Request transaction failed (no refund event) — check " + short(tx)); return; }
      const id = BigInt(log.topics[1]).toString();
      const list = savedRefunds();
      list.push({ id, amount });
      saveRefunds(list);
      $("refund-amount").value = "";
      setStatus("");
      await renderRefunds();
      return;
    }
    setStatus("Receipt still pending — the request will appear after you reload.");
  } catch (e) {
    setStatus("Withdrawal request failed: " + errMsg(e));
  }
}

/* Read each locally-known refund's on-chain state and render countdown / claim. */
async function renderRefunds() {
  const box = $("refund-list");
  const list = savedRefunds();
  box.innerHTML = "";
  if (!list.length) return;
  const keep = [];
  for (const r of list) {
    let row;
    try {
      const out = await rpcCall("eth_call", [{ to: state.cfg.contract_address, data: SEL.getRefund + pad32("0x" + BigInt(r.id).toString(16)) }, "latest"]);
      const words = out.slice(2).match(/.{64}/g) || [];
      const claimableAt = Number(BigInt("0x" + words[3]));
      const claimed = BigInt("0x" + words[4]) === 1n;
      const cancelled = BigInt("0x" + words[5]) === 1n;
      if (claimed || cancelled) continue; // resolved — drop from the local list
      keep.push(r);
      row = document.createElement("div");
      row.className = "keyrow";
      const now = Math.floor(Date.now() / 1000);
      if (claimableAt > now) {
        const mins = Math.ceil((claimableAt - now) / 60);
        row.innerHTML = `<div class="keyrow-info"><b>${r.amount} ${nativeSymbol()}</b>
          <div class="hint">unlocks in ~${mins} min</div></div>`;
      } else {
        row.innerHTML = `<div class="keyrow-info"><b>${r.amount} ${nativeSymbol()}</b>
          <div class="hint">unlocked — claim to receive</div></div>`;
        const btn = document.createElement("button");
        btn.className = "small primary";
        btn.textContent = "Claim";
        btn.onclick = () => claimRefund(r.id);
        row.appendChild(btn);
      }
    } catch {
      keep.push(r); // transient read failure: keep and retry next render
      continue;
    }
    box.appendChild(row);
  }
  saveRefunds(keep);
}

async function claimRefund(id) {
  try {
    await ensureChain();
    setStatus("Confirm the claim in your wallet…");
    const tx = await provider().request({
      method: "eth_sendTransaction",
      params: [{ from: state.wallet, to: state.cfg.contract_address, data: SEL.claimRefund + pad32("0x" + BigInt(id).toString(16)) }],
    });
    setStatus("Claim sent: " + short(tx) + " — FIL arrives after confirmation.");
    for (let i = 0; i < 20; i++) {
      await sleep(10000);
      const rcpt = await rpcCall("eth_getTransactionReceipt", [tx]).catch(() => null);
      if (rcpt) break;
    }
    await renderRefunds();
    await refreshBalance();
    setStatus("");
  } catch (e) {
    setStatus("Claim failed: " + errMsg(e));
  }
}

/* ---------------- models + pricing ---------------- */

/* Per-model pricing (USD per 1M tokens) from the public webconfig, keyed by model
 * id. A synthetic "default" entry mirrors the fallback the biller applies to any
 * model without an explicit catalog row, so the price bar has something to show
 * before the model list loads. */
function buildCatalog(cfg) {
  // Keep the RAW price strings from the server for display (config formats them
  // "0.20"/"0.60"); parsing to float here would strip trailing zeros. No
  // arithmetic is done on these — the bar/table just interpolate them.
  state.catalog = {};
  _catalogIndex = null;   // the normalized lookup must be rebuilt for the new catalog
  for (const m of cfg.models_pricing || []) {
    state.catalog[m.id] = {
      input: m.input_per_mtok || "0",
      output: m.output_per_mtok || "0",
      cache: m.cache_read_per_mtok || "0",
      available: !!m.available,
      // Vision plumbing (see the attachments section): modality gates the attach
      // UI, the window/output pair sizes the token budget. Absent on older
      // gateways, where the UI falls back to a text-only, 8192-window view.
      modality: m.modality || "",
      context_window: m.context_window || 0,
      max_output: m.max_output || 0,
    };
  }
  const d = cfg.prices_usd;
  if (d) {
    state.catalog.default = {
      input: d.default_input_per_mtok || "0",
      output: d.default_output_per_mtok || "0",
      cache: d.default_cache_read_per_mtok || "0",
      available: true,
    };
  }
  renderPricingTable();
  // Populate the model dropdown from the public catalog so real models are
  // selectable even before a key (loadModels refines with live availability once
  // authed). "default" is never offered — it is not a requestable model.
  const ids = Object.keys(state.catalog)
    .filter((id) => id !== "default")
    .sort((a, b) => (state.catalog[b].available === state.catalog[a].available ? 0 : state.catalog[b].available ? 1 : -1));
  if (ids.length) fillModelOptions(ids);
  updateModelPrice();
}

function fillModelOptions(ids) {
  const sel = $("model-select");
  // Canonicalize to the CATALOG spelling before building the options. The picker is
  // filled twice — once from /v1/webconfig (catalog spelling) and again from
  // /v1/models (worker spelling) — and those differ ("/models/Qwen--Qwen3-VL-8B-
  // Instruct" vs "Qwen/Qwen3-VL-8B-Instruct"). Keeping whichever came last made every
  // downstream `catalog[id]` lookup miss, which is how the picker ended up showing a
  // vision model's name while the price line and the attach button behaved as if a
  // text model were selected. Normalizing here fixes all of those at once.
  const seen = new Set();
  const canonical = [];
  for (const id of ids) {
    // Prefer the configured key when this id names a catalog model; otherwise keep
    // the id as given (a model the catalog does not price).
    const key = _catalogIndex && _catalogIndex[normalizeModelID(id)]
      ? (Object.keys(state.catalog).find((k) => k !== "default" && normalizeModelID(k) === normalizeModelID(id)) || id)
      : id;
    if (seen.has(key)) continue;
    seen.add(key);
    canonical.push(key);
  }
  // Restore by REMEMBERED id, not by "is the current value still in the list".
  // Rebuilding the options resets selectedIndex to 0; if the current value is not in
  // the new list the old code simply left that reset in place.
  const cur = sel.value || sel.dataset.want || localStorage.getItem(LS.model) || "";
  sel.innerHTML = "";
  for (const id of canonical) {
    const o = document.createElement("option");
    o.value = id;
    o.textContent = friendlyModelName(id);
    sel.appendChild(o);
  }
  // Match the remembered choice on the normalized key too, so a value remembered
  // under one spelling is still restored when the options arrive in the other.
  const curNorm = normalizeModelID(cur);
  const match = canonical.find((id) => id === cur || normalizeModelID(id) === curNorm);
  if (match) {
    sel.value = match;
  } else if (canonical.length) {
    // No remembered match: prefer a vision model, since attaching images is the
    // capability this picker most often exists to reach.
    const vis = canonical.find((id) => { const e = catalogEntry(id); return e && e.modality === "vision"; });
    sel.value = vis || canonical[0];
  }
  sel.dataset.want = sel.value;
  onModelChanged();
}

function priceFor(modelId) {
  // catalogEntry (not a raw key lookup) so the price line shows the right tier
  // whether the selected id came from the catalog spelling or the live list's.
  return catalogEntry(modelId) || (state.catalog && state.catalog.default) || null;
}

/* The current model's rate, shown both in the sidebar and as the chat-pane bar. */
function updateModelPrice() {
  const id = $("model-select").value;
  const p = priceFor(id);
  if (!p) { $("model-price").textContent = ""; $("model-price-bar").hidden = true; return; }
  const line = `in $${p.input} · cached $${p.cache} · out $${p.output} / 1M tok`;
  $("model-price").textContent = line;
  const bar = $("model-price-bar");
  bar.textContent = `${friendlyModelName(id)} — ${line}`;
  bar.hidden = false;
  updateApiSnippet(); // keep the copy-paste example on the selected model
}

function renderPricingTable() {
  const box = $("pricing-table");
  if (!box) return;
  const ids = Object.keys(state.catalog || {}).filter((id) => id !== "default").sort();
  if (!ids.length) { box.innerHTML = '<p class="hint">Pricing unavailable.</p>'; return; }
  let html = '<table class="pricing"><thead><tr><th>Model</th><th>Input</th><th>Cached</th><th>Output</th></tr></thead><tbody>';
  for (const id of ids) {
    const p = state.catalog[id];
    const dim = p.available ? "" : ' class="offline"';
    html += `<tr${dim}><td title="${esc(id)}">${esc(friendlyModelName(id))}</td><td>$${p.input}</td><td>$${p.cache}</td><td>$${p.output}</td></tr>`;
  }
  html += "</tbody></table>";
  box.innerHTML = html;
}

async function loadModels() {
  try {
    const res = await fetch("/v1/models", { headers: { Authorization: "Bearer " + state.apiKey } });
    if (!res.ok) return;
    const body = await res.json();
    // Only real loaded models; the gateway no longer advertises a "default" alias.
    const ids = (body.data || []).map((m) => m.id).filter((id) => id && id !== "default");
    if (ids.length) fillModelOptions(ids);
    updateModelPrice();
  } catch { /* dropdown keeps whatever the public catalog populated */ }
}

/* Build the outgoing messages array. Turns that carried images keep them, but only
 * the newest turn re-sends the pixels: history images would be re-billed on every
 * follow-up and, with no per-request image cap, would eventually push the prompt
 * past the context window on their own. Older turns carry a text marker instead so
 * the model can still see that a picture was discussed. */
function buildRequestMessages() {
  let lastImgIdx = -1;
  state.messages.forEach((m, i) => { if (m.role === "user" && m.images && m.images.length) lastImgIdx = i; });
  return state.messages.map((m, i) => {
    if (m.role === "user" && m.images && m.images.length) {
      if (i !== lastImgIdx) {
        return { role: "user", content: (m.content ? m.content + " " : "") + "[image omitted from history]" };
      }
      const parts = [];
      if (m.content) parts.push({ type: "text", text: m.content });
      for (const url of m.images) parts.push({ type: "image_url", image_url: { url } });
      return { role: "user", content: parts };
    }
    return { role: m.role, content: m.content };
  });
}

/* ---------------- image attachments ----------------
 *
 * The model's catalog entry decides everything here: `modality` says whether the
 * selected model reads images at all, and `context_window` / `max_output` bound
 * how much can be sent. There is deliberately NO image-count limit — the worker
 * accepts as many as fit, and the real ceiling is the context window, enforced
 * here before anything is uploaded so a request that would be rejected (or, worse,
 * billed almost nothing and answered blind) is never sent.
 *
 * Token accounting, measured against Qwen3-VL on the real hardware:
 *   visual tokens ≈ pixels / 1000   → 512² ≈ 258, 1024² ≈ 1026 per image.
 * Text is estimated at ~4 chars per token, the same heuristic the gateway uses to
 * size its balance reservation. Both are estimates: the authoritative count comes
 * back in `usage.prompt_tokens` after the request, and that is what is billed.
 */
const ATTACH_MAX_BYTES = 7 * 1024 * 1024;   // matches the worker's per-image reject point
const IMG_FALLBACK_CTX = 8192;              // used when the catalog omits the window

/* Catalog lookup that tolerates the two spellings the gateway uses for one model.
 *
 * /v1/webconfig and /v1/catalog key their entries by the CONFIG spelling
 * ("/models/Qwen--Qwen3-VL-8B-Instruct"), while /v1/models lists the worker-reported
 * spelling ("Qwen/Qwen3-VL-8B-Instruct") — the same model, and the gateway accepts
 * either on a request. The picker is filled from both sources (catalog first, then
 * the live list), so a raw `catalog[sel.value]` lookup misses whenever the live list
 * won — price fell back to the default tier and the vision checks read "not a vision
 * model", which is exactly the "name looks right, features are wrong" symptom.
 * Compare on a normalized key instead: drop the "/models/" prefix and treat "--" and
 * "/" as the same separator. */
function normalizeModelID(id) {
  return String(id || "")
    .replace(/^\/?models\//i, "")
    .replace(/--/g, "/")
    .replace(/^\/+/, "")
    .toLowerCase();
}

let _catalogIndex = null;   // normalized id → catalog entry; rebuilt when the catalog changes

function catalogEntry(id) {
  if (!state.catalog) return null;
  if (!_catalogIndex) {
    _catalogIndex = {};
    for (const k of Object.keys(state.catalog)) {
      if (k === "default") continue;
      _catalogIndex[normalizeModelID(k)] = state.catalog[k];
    }
  }
  return state.catalog[id] || _catalogIndex[normalizeModelID(id)] || null;
}

function modelMeta() {
  return catalogEntry($("model-select").value);
}

/* Whether the currently selected model accepts image parts. */
function modelIsVision() {
  const m = modelMeta();
  return !!(m && m.modality === "vision");
}

function ctxWindow() {
  const m = modelMeta();
  return (m && m.context_window) || IMG_FALLBACK_CTX;
}

function outReserve() {
  const m = modelMeta();
  return (m && m.max_output) || 0;
}

/* Input budget: everything except the space reserved for the reply. */
function availTokens() { return Math.max(0, ctxWindow() - outReserve()); }

function imgTokens(a) { return a.tokens || Math.round((a.w * a.h) / 1000); }
function attachTokens() { return state.attach.reduce((n, a) => n + imgTokens(a), 0); }

/* Prompt text already in the conversation, plus what is typed now. Cheap and
 * deliberately conservative — over-counting only makes the bar warn earlier. */
function promptTextTokens() {
  let chars = $("input").value.length;
  for (const m of state.messages) {
    if (typeof m.content === "string") chars += m.content.length;
    else if (Array.isArray(m.content)) for (const p of m.content) if (p.type === "text") chars += (p.text || "").length;
  }
  return Math.round(chars / 4) + 8;
}

function usedTokens() { return attachTokens() + promptTextTokens(); }
function overBudget() { return Math.max(0, usedTokens() - availTokens()); }

/* The size ceiling currently selected, or 0 for "keep the original". Small images
 * are never enlarged: blowing up a 90×80 icon to 1024² invents pixels the model
 * cannot read and bills ~1k tokens for them. */
function targetSide(bmp) {
  const want = parseInt($("img-size").value, 10) || 0;
  if (!want) return 0;
  return Math.max(bmp.width, bmp.height) <= want ? 0 : want;
}

function loadBitmap(file) {
  if (window.createImageBitmap) return createImageBitmap(file).catch(() => loadViaImg(file));
  return loadViaImg(file);
}

function loadViaImg(file) {
  return new Promise((res, rej) => {
    const url = URL.createObjectURL(file);
    const im = new Image();
    im.onload = () => { URL.revokeObjectURL(url); res(im); };
    im.onerror = () => { URL.revokeObjectURL(url); rej(new Error("cannot decode this image")); };
    im.src = url;
  });
}

/* Re-encode every kept original at the current size setting. Keeping the decoded
 * bitmap is what lets a size change apply to images already attached — without it
 * the setting would only affect the next file chosen. */
async function reencodeAttachments() {
  const out = [];
  for (const src of state.attachSrc) {
    const bmp = src.bmp;
    const side = targetSide(bmp);
    let w = bmp.width, h = bmp.height;
    if (side > 0) {
      const scale = side / Math.max(w, h);
      w = Math.max(1, Math.round(w * scale));
      h = Math.max(1, Math.round(h * scale));
    }
    const cv = document.createElement("canvas");
    cv.width = w; cv.height = h;
    const ctx = cv.getContext("2d");
    ctx.fillStyle = "#fff";   // JPEG has no alpha: transparent pixels would go black
    ctx.fillRect(0, 0, w, h);
    ctx.drawImage(bmp, 0, 0, w, h);
    let dataUrl = cv.toDataURL("image/jpeg", 0.9);
    if (dataUrl.length > ATTACH_MAX_BYTES * 1.37) dataUrl = cv.toDataURL("image/jpeg", 0.7);
    const bytes = Math.round((dataUrl.length - dataUrl.indexOf(",") - 1) * 0.75);
    if (bytes > ATTACH_MAX_BYTES) {
      throw new Error(src.name + " is still " + (bytes / 1048576).toFixed(1) + " MiB after resizing (limit 7 MiB)");
    }
    out.push({ dataUrl, w, h, name: src.name, bytes, tokens: Math.round((w * h) / 1000) });
  }
  state.attach = out;
  renderAttachments();
}

async function addFiles(files) {
  if (!modelIsVision()) {
    setStatus("The selected model does not read images — pick a vision model first.");
    return;
  }
  const list = Array.from(files).filter((f) => {
    if (!f.type.startsWith("image/")) { setStatus("Skipped a non-image file: " + (f.name || "?")); return false; }
    return true;
  });
  if (!list.length) return;
  // Snapshot before mutating: a rejected addition must undo ONLY the new files and
  // leave whatever was already staged in place. Truncating the arrays by count
  // instead loses the earlier attachments too (they all get re-encoded together).
  const prevSrc = state.attachSrc.slice();
  const prevAtt = state.attach.slice();
  try {
    for (const f of list) state.attachSrc.push({ bmp: await loadBitmap(f), name: f.name || "pasted.png" });
    await reencodeAttachments();
  } catch (e) {
    state.attachSrc = prevSrc;
    state.attach = prevAtt;
    renderAttachments();
    setStatus(errMsg(e));
    return;
  }
  const over = overBudget();
  if (over > 0) {
    // Refuse the addition outright rather than let Send produce a guaranteed 400.
    state.attachSrc = prevSrc;
    state.attach = prevAtt;
    renderAttachments();
    setStatus("That would exceed the model's input budget by ~" + over + " tokens. "
      + "Remove an image, choose a smaller size, or start a new conversation.");
    return;
  }
  setStatus(state.attach.length + " image" + (state.attach.length > 1 ? "s" : "") + " attached ("
    + attachTokens() + " of " + availTokens() + " input tokens used).");
}

/* Switching models can invalidate staged images: a text-only model cannot accept
 * them, and a different vision model may have a different window/budget. Clearing
 * is the honest move — silently sending them would be a guaranteed 400. */
function onModelChanged() {
  const vision = modelIsVision();
  $("btn-attach").hidden = !vision;
  if (!vision && state.attach.length) {
    state.attach = [];
    state.attachSrc = [];
    setStatus("The newly selected model does not read images, so the attached "
      + "image(s) were removed.");
  }
  renderAttachments();
}

function renderAttachments() {
  const strip = $("attach-strip");
  const box = $("attach-thumbs");
  const has = state.attach.length > 0;
  strip.hidden = !has;
  box.innerHTML = "";
  state.attach.forEach((a, i) => {
    const d = document.createElement("div");
    d.className = "attach-thumb";
    d.innerHTML = '<img src="' + a.dataUrl + '" alt="">'
      + '<button class="attach-x" title="Remove">×</button>'
      + '<div class="attach-tok">' + a.w + "×" + a.h + " · " + imgTokens(a) + " tok</div>";
    d.querySelector(".attach-x").onclick = () => {
      state.attach.splice(i, 1);
      state.attachSrc.splice(i, 1);
      renderAttachments();
      setStatus(state.attach.length
        ? state.attach.length + " image" + (state.attach.length > 1 ? "s" : "") + " attached ("
          + attachTokens() + " of " + availTokens() + " input tokens used)."
        : "Image removed.");
    };
    box.appendChild(d);
  });
  renderBudget();
  updateImgSizeHint();
  // A size change re-encodes in place, so re-state the totals; without this the
  // status line keeps quoting the count from before the change.
  if (has) $("attach-count").textContent = state.attach.length + " image"
    + (state.attach.length > 1 ? "s" : "") + " · " + attachTokens() + " tok";
}

function renderBudget() {
  const el = $("attach-budget");
  if (!el) return;
  const avail = availTokens(), used = usedTokens(), over = overBudget();
  const pct = avail ? Math.min(100, (used / avail) * 100) : 0;
  const cls = over > 0 ? "over" : (pct >= 80 ? "warn" : "");
  el.innerHTML = '<span class="budget"><i class="' + cls + '" style="width:' + pct.toFixed(1) + '%"></i></span> '
    + used + " / " + avail + " tok"
    + (over > 0 ? ' — <b>over by ' + over + '</b>' : "");
  // Send stays disabled while over budget: there is no request that would succeed.
  $("btn-send").disabled = state.streaming || !state.apiKey || over > 0;
}

function updateImgSizeHint() {
  const row = $("img-size-row"), hint = $("img-size-hint"), vh = $("vision-hint");
  const on = modelIsVision();
  row.hidden = !on;
  hint.hidden = !on;
  if (!on) {
    // Point out how to get the feature instead of silently hiding it: a user who
    // cannot find the attach button has no way to learn it depends on the model.
    const hasVision = Object.keys(state.catalog || {}).some((id) => state.catalog[id].modality === "vision");
    vh.hidden = !hasVision;
    if (hasVision) {
      vh.textContent = "Image input is available on the vision model — pick it above to attach images.";
    }
    return;
  }
  vh.hidden = true;
  let w = 0, h = 0;
  for (const s of state.attachSrc) {
    if (s.bmp.width * s.bmp.height > w * h) { w = s.bmp.width; h = s.bmp.height; }
  }
  const long = Math.max(w, h);
  const desc = (want) => {
    if (!want || !long || long <= want) return long ? ("as-is " + w + "×" + h) : "as-is";
    const scale = want / long;
    const tw = Math.round(w * scale), th = Math.round(h * scale);
    return tw + "×" + th + " ≈ " + Math.round((tw * th) / 1000) + " tok each";
  };
  const opts = $("img-size").options;
  opts[0].textContent = "≤512 px" + (long ? " (" + desc(512) + ")" : "");
  opts[1].textContent = "≤1024 px" + (long ? " (" + desc(1024) + ")" : "");
  opts[2].textContent = "original" + (long ? " (" + desc(0) + ")" : "");
  hint.textContent = "Images are sent as prompt tokens (~pixels/1000; 1024² ≈ 1030 tok). "
    + "This model takes " + availTokens() + " input tokens per request.";
}

/* ---------------- chat ---------------- */

async function send() {
  const text = $("input").value.trim();
  const imgs = state.attach.map((a) => a.dataUrl);
  if ((!text && !imgs.length) || !state.apiKey || state.streaming) return;
  const model = $("model-select").value;
  if (!model) {
    setStatus("No model is available right now — pick one once a model comes online.");
    return;
  }
  if (overBudget()) {
    // Nothing that follows can succeed: the prompt would not fit.
    setStatus("This request is ~" + overBudget() + " tokens over the model's input budget. "
      + "Remove an image, pick a smaller size, or start a new conversation.");
    return;
  }
  $("input").value = "";
  state.messages.push({ role: "user", content: text, images: imgs });
  state.attach = [];
  state.attachSrc = [];
  renderAttachments();
  const assistant = { role: "assistant", content: "", meta: {} };
  state.messages.push(assistant);
  persist();
  renderMessages();

  state.streaming = true;
  state.abort = new AbortController();
  $("btn-stop").hidden = false;
  renderKey();
  setStatus("Waiting for a GPU…");

  try {
    // A 503 usually means providers are mining or a model switch (~2-3 min for
    // the big models) is in progress; both self-heal, so retry with backoff for
    // up to ~4 minutes instead of dumping the error on the user. Stop aborts.
    let res;
    for (let attempt = 0; ; attempt++) {
      res = await fetch("/v1/chat/completions", {
        method: "POST",
        signal: state.abort.signal,
        headers: {
          "Content-Type": "application/json",
          Authorization: "Bearer " + state.apiKey,
          "X-Session-Id": state.sessionId,     // prefix-cache affinity to one worker
          "X-OM-Receipt-Req": "1",             // ask for the signed billing receipt event
        },
        body: JSON.stringify({
          model,
          stream: true,
          enable_thinking: state.thinking,
          messages: buildRequestMessages().slice(0, -1),   // drop the empty streaming placeholder
        }),
      });
      if ((res.status !== 503 && res.status !== 502) || attempt >= 12) break;
      setStatus("Model is loading or providers are mining — retrying (" + (attempt + 1) + ")…");
      await new Promise((ok) => setTimeout(ok, 20000));
      if (state.abort.signal.aborted) throw Object.assign(new Error("aborted"), { name: "AbortError" });
    }
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      assistant.content = friendlyHTTPError(res.status, errBody(body));
      assistant.meta.error = true;
      return;
    }
    await readSSE(res.body, assistant);
  } catch (e) {
    if (e && e.name === "AbortError") {
      // User pressed Stop: delivered text stays; billing already meters exactly
      // the delivered part (client-gone accounting on the gateway).
      assistant.meta.stopped = true;
    } else {
      assistant.content = assistant.content || "Request failed: " + errMsg(e);
      assistant.meta.error = true;
    }
  } finally {
    state.streaming = false;
    state.abort = null;
    $("btn-stop").hidden = true;
    persist();
    renderMessages();
    renderKey();
    setStatus("");
    refreshMe(); // spend just changed
  }
}

async function readSSE(bodyStream, assistant) {
  const reader = bodyStream.getReader();
  const decoder = new TextDecoder();
  let buf = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    let idx;
    while ((idx = buf.indexOf("\n\n")) >= 0) {
      const event = buf.slice(0, idx);
      buf = buf.slice(idx + 2);
      for (const line of event.split("\n")) {
        if (!line.startsWith("data:")) continue;
        const payload = line.slice(5).trim();
        if (payload === "[DONE]") continue;
        let obj;
        try { obj = JSON.parse(payload); } catch { continue; }
        if (obj.om_receipt) {
          assistant.meta.receipt = obj.om_receipt;
          continue;
        }
        if (obj.error) {
          assistant.meta.error = true;
          assistant.content += (assistant.content ? "\n\n" : "") + "⚠ " + (obj.error.message || JSON.stringify(obj.error));
          continue;
        }
        const delta = obj.choices && obj.choices[0] && obj.choices[0].delta;
        if (delta && delta.reasoning_content) {
          assistant.reasoning = (assistant.reasoning || "") + delta.reasoning_content;
          renderMessages(true);
        }
        if (delta && delta.content) {
          assistant.content += delta.content;
          renderMessages(true);
          setStatus("Streaming…");
        }
      }
    }
  }
}

function friendlyHTTPError(status, msg) {
  if (status === 402) return "⚠ Insufficient balance — deposit some " + nativeSymbol() + " in the sidebar, wait ~1 min for the chain read, and retry.";
  if (status === 401) return "⚠ The API key was rejected. Re-register or paste a valid key.";
  if (status === 503 || status === 502) return "⚠ Still unavailable after several retries — providers are mining (storage proofs take priority), a large model is loading, or the provider hit an error. Try again in a few minutes.";
  return "⚠ " + status + ": " + msg;
}

/* ---------------- rendering ---------------- */

function renderMessages(streaming) {
  const box = $("messages");
  const stick = box.scrollHeight - box.scrollTop - box.clientHeight < 60;
  $("welcome").hidden = state.messages.length > 0;
  for (const el of box.querySelectorAll(".msg:not(.system)")) el.remove();
  for (const m of state.messages) {
    const div = document.createElement("div");
    div.className = "msg " + m.role + (m.meta && m.meta.error ? " error" : "");
    div.innerHTML = mdLite(m.content || (streaming ? "▌" : ""));
    if (m.images && m.images.length) {
      // Show what was actually sent. Only the newest turn's pixels go back to the
      // model, but every turn keeps its thumbnails so the transcript reads correctly.
      const strip = document.createElement("div");
      strip.className = "msg-imgs";
      for (const url of m.images) {
        const im = document.createElement("img");
        im.src = url; im.alt = "attached image";
        strip.appendChild(im);
      }
      div.prepend(strip);
    } else if (m.imageCount) {
      // Restored transcript: the bytes were never stored (see persist), so say so
      // rather than showing a bubble that reads as if it were plain text.
      const strip = document.createElement("div");
      strip.className = "msg-imgs";
      const note = document.createElement("span");
      note.className = "hint";
      note.textContent = "[" + m.imageCount + " image" + (m.imageCount > 1 ? "s" : "")
        + " sent in this message — not kept in browser history]";
      strip.appendChild(note);
      div.prepend(strip);
    }
    if (m.reasoning) {
      const th = document.createElement("details");
      th.className = "think";
      // Keep it open while the reasoning is still streaming in; collapse once
      // the actual answer starts so it reads as a footnote, not the reply.
      th.open = streaming && !m.content;
      th.innerHTML = "<summary>Thinking</summary>";
      const body = document.createElement("div");
      body.className = "think-body";
      body.textContent = m.reasoning;
      th.appendChild(body);
      div.prepend(th);
    }
    if (m.meta && m.meta.stopped) {
      const s = document.createElement("div");
      s.className = "hint";
      s.textContent = "⏹ stopped — you are billed only for the text above";
      div.appendChild(s);
    }
    if (m.meta && m.meta.receipt) div.appendChild(receiptChip(m.meta.receipt));
    box.appendChild(div);
  }
  if (stick) box.scrollTop = box.scrollHeight;
}

function receiptChip(r) {
  const chip = document.createElement("div");
  chip.className = "receipt";
  const rid = r.request_id || "";
  let tokens = "";
  void 0;
  if (r.prompt_tokens != null && r.completion_tokens != null) {
    // Tokens only — the dollar amount is filled in from /v1/me (the gateway's
    // own billing figure) right after the reply lands. The chip used to price
    // itself with the UI's display rates and drifted from the account panel the
    // moment the rates differed from the billed model's (a 32B request showed
    // $0.001343 here and $0.001931 there — same request, two answers). One
    // caliber: the UI never multiplies price × tokens on its own.
    tokens = `${r.prompt_tokens}+${r.completion_tokens} tok<span class="chip-cost" data-rid="${esc(rid)}"></span> · `;
  }
  const base = state.cfg && state.cfg.public_query_url;
  if (base && rid) {
    const url = `${base}/api/v1/receipt-proof/${encodeURIComponent(rid)}`;
    chip.innerHTML = `${tokens}signed receipt <a href="${url}" class="mono">${esc(short(rid))}</a>`;
    // In-page viewer instead of a raw-JSON tab: right after a reply — the moment
    // people actually click — the batch is usually not on-chain yet, and the raw
    // endpoint answer there LOOKS like an error about their money. The viewer
    // turns that state into "signed ✓, batch in ~N min" with auto-refresh.
    chip.querySelector("a").addEventListener("click", (e) => {
      e.preventDefault();
      showReceiptViewer(rid, url);
    });
  } else {
    chip.innerHTML = `${tokens}signed receipt <span class="mono" title="${esc(rid)}">${esc(short(rid))}</span>`;
  }
  return chip;
}

/* Minimal safe formatting: escape everything, then fenced code blocks and inline
 * code/bold. Not a markdown engine on purpose. */
function mdLite(text) {
  const parts = esc(text).split(/```(?:[a-zA-Z0-9_-]*\n)?/);
  let html = "";
  for (let i = 0; i < parts.length; i++) {
    if (i % 2 === 1) html += "<pre>" + parts[i].replace(/\n$/, "") + "</pre>";
    else html += parts[i]
      .replace(/`([^`\n]+)`/g, "<code>$1</code>")
      .replace(/\*\*([^*\n]+)\*\*/g, "<b>$1</b>")
      .replace(/\n/g, "<br>");
  }
  return html;
}

/* ---------------- helpers ---------------- */

function esc(s) { return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"); }
function short(s) { return s.length > 14 ? s.slice(0, 8) + "…" + s.slice(-4) : s; }
function pad32(hex) { return hex.replace(/^0x/, "").toLowerCase().padStart(64, "0"); }
function utf8ToHex(s) { return Array.from(new TextEncoder().encode(s), (b) => b.toString(16).padStart(2, "0")).join(""); }
function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }
function nativeSymbol() {
  const c = state.cfg && state.cfg.chain;
  return (c && c.nativeCurrency && c.nativeCurrency.symbol) || "FIL";
}
function formatFIL(wei) {
  const whole = wei / 10n ** 18n;
  const frac = ((wei % 10n ** 18n) * 10n ** 6n) / 10n ** 18n; // 6 decimals shown
  return `${whole}.${frac.toString().padStart(6, "0")}`;
}
function errMsg(e) { return (e && (e.message || e.toString())) || "unknown error"; }
function errBody(b) { return (b && b.error && (b.error.message || JSON.stringify(b.error))) || JSON.stringify(b); }
function setStatus(s) { $("statusline").textContent = s; }
function persist() {
  // Stored WITHOUT the image bytes. The base64 of even one screenshot is larger
  // than the whole transcript budget (localStorage is ~5 MB per origin), so keeping
  // the DataURLs here would silently wipe the conversation the moment an image was
  // attached. A count is stored instead, so the restored transcript can say "an
  // image was sent here" without pretending to have it.
  const slim = state.messages.slice(-80).map((m) => {
    if (!m.images || !m.images.length) return m;
    const { images, ...rest } = m;
    return { ...rest, imageCount: images.length };
  });
  try {
    localStorage.setItem(LS.msgs, JSON.stringify(slim));
  } catch (e) {
    // Quota can still be hit by very long text transcripts; dropping the oldest
    // half is better than losing the whole conversation to an exception.
    try { localStorage.setItem(LS.msgs, JSON.stringify(slim.slice(-40))); } catch { /* give up */ }
  }
  localStorage.setItem(LS.session, state.sessionId);
}

boot();


/* ---- receipt viewer -------------------------------------------------------
 * One overlay, three states mirroring the endpoint:
 *   200 → settled: key fields + collapsible raw proof JSON;
 *   202 → pending: "recorded & worker-signed, batch in ~N min", auto-refetch;
 *   404 → unknown id.
 */
let receiptTimer = null;
function showReceiptViewer(rid, url) {
  let ov = document.getElementById("receipt-overlay");
  if (!ov) {
    ov = document.createElement("div");
    ov.id = "receipt-overlay";
    ov.innerHTML = `<div id="receipt-card"><button id="receipt-close">×</button><div id="receipt-body"></div></div>`;
    document.body.appendChild(ov);
    ov.addEventListener("click", (e) => { if (e.target === ov) closeReceiptViewer(); });
    ov.querySelector("#receipt-close").addEventListener("click", closeReceiptViewer);
  }
  ov.style.display = "flex";
  renderReceipt(rid, url);
}
function closeReceiptViewer() {
  const ov = document.getElementById("receipt-overlay");
  if (ov) ov.style.display = "none";
  if (receiptTimer) { clearTimeout(receiptTimer); receiptTimer = null; }
}
async function renderReceipt(rid, url) {
  const body = document.getElementById("receipt-body");
  body.innerHTML = `<p class="dim">Fetching receipt ${esc(short(rid))}…</p>`;
  let resp, data;
  try {
    resp = await fetch(url);
    data = await resp.json();
  } catch (e) {
    body.innerHTML = `<p><b>Could not reach the public query endpoint.</b></p><p class="dim">${esc(String(e))}</p>`;
    return;
  }
  if (resp.status === 202) {
    const eta = Math.max(0, data.next_settlement_eta_sec | 0);
    const mins = Math.floor(eta / 60), secs = eta % 60;
    const signed = data.worker_receipt && data.worker_receipt.verified;
    body.innerHTML =
      `<h3>Receipt recorded — settlement on the way</h3>` +
      `<p>✅ Your request <span class="mono">${esc(rid)}</span> is billed${signed ? " and carries a <b>worker-signed receipt</b> (verifiable right now)" : ""}.</p>` +
      `<p>⏳ The on-chain settlement batch is committed on a fixed cycle — next pass in about <b>${mins}m ${secs}s</b>. ` +
      `The full Merkle inclusion proof appears here the moment it lands; this view refreshes itself.</p>` +
      (data.model ? `<p class="dim">model ${esc(String(data.model))} · ${esc(String(data.total_tokens))} tokens · ${esc(String(data.request_time || ""))}</p>` : "") +
      `<details><summary>raw response</summary><pre>${esc(JSON.stringify(data, null, 2))}</pre></details>`;
    if (receiptTimer) clearTimeout(receiptTimer);
    receiptTimer = setTimeout(() => renderReceipt(rid, url), Math.min(Math.max(eta, 15), 120) * 1000);
    return;
  }
  if (resp.ok) {
    body.innerHTML =
      `<h3>Settled on-chain ✓</h3>` +
      `<p>Request <span class="mono">${esc(rid)}</span> is committed in settlement batch ` +
      `<span class="mono">${esc(String(data.settlement_id ?? ""))}</span> (tx <span class="mono">${esc(short(String(data.tx_hash || "")))}</span>, block ${esc(String(data.block_number ?? ""))}).</p>` +
      `<p class="dim">Verify offline with verify-receipt.py — it checks the worker signature, content hashes, Merkle inclusion, the on-chain details hash, and the billed amount.</p>` +
      `<details open><summary>full proof JSON</summary><pre>${esc(JSON.stringify(data, null, 2))}</pre></details>`;
    return;
  }
  body.innerHTML =
    `<h3>No record found</h3>` +
    `<p>The endpoint has no billing record for <span class="mono">${esc(rid)}</span> (HTTP ${resp.status}).</p>` +
    `<p class="dim">${esc(data && data.error ? data.error : "")}</p>`;
}
