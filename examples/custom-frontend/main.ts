import type Hls from "hls.js";
import { createClient, type PlaybackSource, type VideoItem } from "../../sdk/src/index";
import "./style.css";

const client = createClient();
const element = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const status = element("status");
const login = element<HTMLFormElement>("login");
const dialog = element<HTMLDialogElement>("player");
const video = dialog.querySelector("video")!;
const sources = element<HTMLSelectElement>("sources");
const settings = element<HTMLFormElement>("settings");
let setupRequired = false;
let page = 1;
let query = "";
let listingRequest: AbortController | undefined;
let playbackRequest: AbortController | undefined;
let hls: Hls | undefined;
let currentVideo: VideoItem | undefined;
let selectedSource: PlaybackSource | undefined;
let poll: ReturnType<typeof setTimeout> | undefined;

function message(error: unknown) { return error instanceof Error ? error.message : "请求失败，请重试"; }
function theme(value: string) { document.documentElement.dataset.theme = value; }

async function run(action: () => Promise<void>, button?: HTMLButtonElement) {
  if (button) button.disabled = true;
  status.textContent = "";
  try { await action(); } catch (error) { status.textContent = message(error); }
  finally { if (button) button.disabled = false; }
}

async function loadLibrary() {
  listingRequest?.abort();
  const request = listingRequest = new AbortController();
  try {
    const result = await client.videos.fetchListing(page, 12, { q: query }, { signal: request.signal });
    if (request.signal.aborted) return;
    const cards = result.items.map(item => {
      const card = document.createElement("button");
      card.className = "card";
      const image = document.createElement("img");
      image.src = client.resourceURL(item.thumbnail);
      image.alt = "";
      image.loading = "lazy";
      const title = document.createElement("strong");
      title.textContent = item.title;
      const metadata = document.createElement("span");
      metadata.textContent = `${item.duration} · ${item.author || "我的收藏"}`;
      card.append(image, title, metadata);
      card.onclick = () => { currentVideo = item; element("video-title").textContent = item.title; dialog.showModal(); void loadPlayback(); };
      return card;
    });
    element("videos").replaceChildren(...cards);
    element("page").textContent = `${page} / ${Math.max(1, Math.ceil(result.total / 12))}`;
    element<HTMLButtonElement>("previous").disabled = page <= 1;
    element<HTMLButtonElement>("next").disabled = page * 12 >= result.total;
    status.textContent = result.total ? `${result.total} 部视频` : "还没有找到视频";
  } catch (error) { if (!request.signal.aborted) throw error; }
}

function stopPlayback() {
  clearTimeout(poll);
  playbackRequest?.abort();
  hls?.destroy();
  hls = undefined;
  video.pause();
  video.removeAttribute("src");
  video.load();
}

async function loadPlayback(sourceID = "original", options: { prepare?: boolean; refresh?: boolean; deadline?: number } = {}) {
  stopPlayback();
  const request = playbackRequest = new AbortController();
  const playbackStatus = element("playback-status");
  playbackStatus.textContent = "正在获取播放地址…";
  element("prepare").hidden = true;
  try {
    const manifest = options.prepare && selectedSource?.prepareUrl
      ? await client.playback.prepare(selectedSource.prepareUrl, request.signal)
      : await client.playback.get(currentVideo!.id, { source: sourceID, refresh: options.refresh, signal: request.signal });
    if (request.signal.aborted) return;
    selectedSource = manifest.sources.find(source => source.id === sourceID && source.url)
      ?? (sourceID === "cloud" ? manifest.sources.find(source => source.kind === "transcoded" && source.url) : undefined)
      ?? manifest.sources.find(source => source.id === sourceID);
    sources.replaceChildren(...manifest.sources.map(source => new Option(source.label, source.id)));
    if (selectedSource) sources.value = selectedSource.id;
    const source = selectedSource;
    if (!source || source.status !== "ready" || !source.url) {
      playbackStatus.textContent = source?.message || "该播放源尚未就绪";
      element("prepare").hidden = !source?.prepareUrl;
      if (source?.status === "processing" && (!source.prepareUrl || options.prepare || options.deadline)) {
        const deadline = options.deadline ?? Date.now() + 5 * 60_000;
        element("prepare").hidden = true;
        if (Date.now() < deadline) poll = setTimeout(() => void loadPlayback(sourceID, { deadline }), 15000);
        else playbackStatus.textContent = "视频仍在准备中，请稍后刷新播放地址";
      }
      return;
    }
    const url = client.resourceURL(source.url);
    if (source.protocol === "hls" && !video.canPlayType("application/vnd.apple.mpegurl")) {
      const { default: Hls } = await import("hls.js/light");
      if (request.signal.aborted) return;
      if (!Hls.isSupported()) throw new Error("当前浏览器不支持此播放源，请选择原画播放");
      hls = new Hls();
      hls.on(Hls.Events.ERROR, (_event, data) => { if (data.fatal) playbackStatus.textContent = "播放失败，请刷新地址或选择其他播放源"; });
      hls.loadSource(url);
      hls.attachMedia(video);
    } else video.src = url;
    playbackStatus.textContent = "";
    await video.play().catch(() => { if (!request.signal.aborted) playbackStatus.textContent = "点击播放按钮开始观看"; });
  } catch (error) { if (!request.signal.aborted) playbackStatus.textContent = message(error); }
}

async function openSession() {
  const session = await client.auth.me();
  login.hidden = session.authenticated;
  element("library").hidden = !session.authenticated;
  element("logout").hidden = !session.authenticated;
  element("management").hidden = !session.authenticated || session.role !== "admin";
  if (!session.authenticated) return;
  await loadLibrary();
  if (session.role === "admin") {
    const [drives, preferences] = await Promise.all([client.admin.listDrives(), client.admin.getSettings()]);
    element("drives").replaceChildren(...drives.map(drive => { const row = document.createElement("li"); row.textContent = `${drive.name} · ${drive.kind}`; return row; }));
    (settings.elements.namedItem("theme") as HTMLSelectElement).value = preferences.theme;
  }
}

login.onsubmit = event => {
  event.preventDefault();
  const data = new FormData(login);
  void run(async () => {
    const authenticate = setupRequired ? client.auth.setupAdmin : client.auth.login;
    await authenticate(String(data.get("username")), String(data.get("password")));
    login.reset();
    setupRequired = false;
    await openSession();
  }, login.querySelector("button")!);
};
element("logout").onclick = () => void run(async () => {
  await client.auth.logout();
  listingRequest?.abort();
  dialog.close();
  stopPlayback();
  element("videos").replaceChildren();
  element("drives").replaceChildren();
  page = 1;
  await openSession();
});
element<HTMLFormElement>("search").onsubmit = event => { event.preventDefault(); query = String(new FormData(event.currentTarget as HTMLFormElement).get("q") || ""); page = 1; void run(loadLibrary); };
element("previous").onclick = () => { page = Math.max(1, page - 1); void run(loadLibrary); };
element("next").onclick = () => { page++; void run(loadLibrary); };
element("close-player").onclick = () => dialog.close();
dialog.onclose = stopPlayback;
sources.onchange = () => void loadPlayback(sources.value);
element("prepare").onclick = () => void loadPlayback(sources.value, { prepare: true });
element("retry").onclick = () => void loadPlayback(sources.value, { refresh: true });
settings.onsubmit = event => {
  event.preventDefault();
  void run(async () => {
    const value = new FormData(settings).get("theme") === "light" ? "light" : "dark";
    const saved = await client.admin.updateSettings({ theme: value });
    theme(saved.theme);
    status.textContent = "设置已保存";
  }, settings.querySelector("button")!);
};

void run(async () => {
  const info = await client.server();
  if (!info.apiVersion.startsWith("1.")) throw new Error("此客户端需要 API v1");
  const [setup, preferences] = await Promise.all([client.auth.setupStatus(), client.settings.theme()]);
  theme(preferences.theme);
  setupRequired = setup.required;
  element("login-title").textContent = setupRequired ? "创建管理员账户" : "登录你的影库";
  await openSession();
});
