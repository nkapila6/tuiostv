// Runs in every frame. Stays quiet until a video plays, then records that
// video in short WebM slices and sends them to the tuios-pip viewer through
// the helper, and takes controls back.
//
// Not WebRTC: Firefox only gathers ICE candidates on the default-route
// interface (a VPN's utun, often), and a Mac can't reach itself there.
(() => {
  // the viewer page plays a video too; sharing it back would loop
  if (document.querySelector('meta[name="tuios-pip"]')) return;
  const api = globalThis.chrome ?? globalThis.browser;
  // randomUUID is missing on plain http pages
  const id = crypto.randomUUID?.() ?? Math.random().toString(36).slice(2);
  const SLICE_MS = 50;
  let video = null;
  let rec = null;
  let stateTimer = null;
  let polling = false;
  // bumps on every new recording; the viewer starts a fresh decoder per gen
  let gen = 0;
  const streams = new WeakMap();

  const call = (m) =>
    new Promise((ok) => {
      try {
        api.runtime.sendMessage(m, (r) => ok(r || { ok: false }));
      } catch {
        // extension was reloaded under us; this copy is dead
        ok({ ok: false, dead: true });
      }
    });
  // one at a time: parallel POSTs reach the helper out of order, and the
  // viewer needs the slices in order
  let outbox = Promise.resolve();
  const send = (msg) => (outbox = outbox.then(() => call({ op: "msg", id, msg })));

  const title = () => navigator.mediaSession?.metadata?.title || document.title || location.hostname;
  // playing: true only when this tab just started, which makes it the one shown
  const hello = (playing) =>
    send({
      type: "hello",
      title: title(),
      host: location.hostname,
      playing,
      paused: !video || video.paused,
      // the launcher sizes the popup to this so there are no black bars
      w: video?.videoWidth || 0,
      h: video?.videoHeight || 0,
    });

  // Firefox's mozCaptureStream takes the element's sound into the stream, so
  // play the audio tracks back here or the tab goes silent.
  function capture(v) {
    if (streams.has(v)) return streams.get(v);
    let stream;
    if (v.captureStream) {
      stream = v.captureStream();
    } else {
      stream = v.mozCaptureStream();
      const out = new Audio();
      out.srcObject = new MediaStream(stream.getAudioTracks());
      const sync = () => { out.volume = v.volume; out.muted = v.muted; };
      sync();
      v.addEventListener("volumechange", sync);
      stream.addEventListener("addtrack", (e) => {
        if (e.track.kind === "audio") out.srcObject = new MediaStream(stream.getAudioTracks());
      });
      out.play().catch(() => {});
    }
    // a new source on the same element (next video) arrives as a new track,
    // and a recorder can't switch tracks, so start over
    stream.addEventListener("addtrack", (e) => {
      if (e.track.kind === "video" && rec) start();
    });
    streams.set(v, stream);
    return stream;
  }

  function stop() {
    clearInterval(stateTimer);
    stateTimer = null;
    if (rec && rec.state !== "inactive") rec.stop();
    rec = null;
  }

  const toBase64 = (blob) =>
    new Promise((ok) => {
      const r = new FileReader();
      r.onload = () => ok(String(r.result).split(",", 2)[1] || "");
      r.readAsDataURL(blob);
    });

  function start() {
    stop();
    if (!video) return;
    let stream;
    try {
      stream = capture(video);
    } catch (e) {
      // DRM or a cross-origin source without CORS
      send({ type: "state", title: `Can't share this video: ${e.name}`, t: 0, dur: 0, paused: true });
      return;
    }
    const track = stream.getVideoTracks().filter((t) => t.readyState === "live").at(-1);
    if (!track) {
      // Firefox adds the tracks a moment after capture starts
      setTimeout(() => { if (video && !rec) start(); }, 300);
      return;
    }
    const mime = ["video/webm;codecs=vp8", "video/webm;codecs=vp9", "video/webm"].find((m) => MediaRecorder.isTypeSupported(m));
    const my = ++gen;
    // video only: the sound keeps playing in the tab
    rec = new MediaRecorder(new MediaStream([track]), { mimeType: mime, videoBitsPerSecond: 4_000_000 });
    send({ type: "begin", gen: my, mime });
    rec.ondataavailable = async (e) => {
      if (my !== gen || !e.data.size) return;
      send({ type: "chunk", gen: my, data: await toBase64(e.data) });
    };
    rec.start(SLICE_MS);
    const tick = () => {
      if (!video) return;
      send({ type: "state", title: title(), t: video.currentTime, dur: video.duration, paused: video.paused });
    };
    tick();
    stateTimer = setInterval(tick, 1000);
  }

  function control(m) {
    if (!video) return;
    if (m.action === "toggle") video.paused ? video.play() : video.pause();
    else if (m.action === "seek") video.currentTime = Math.max(0, video.currentTime + m.by);
    else if (m.action === "seekTo") video.currentTime = m.t;
  }

  function handle(m) {
    switch (m.type) {
      case "start": start(); break;
      case "stop": stop(); break;
      case "cmd": control(m); break;
    }
  }

  async function poll() {
    if (polling) return;
    polling = true;
    while (video) {
      const r = await call({ op: "poll", id });
      if (r.dead) return;
      if (!r.ok) {
        // helper not running; check back without hammering it
        await new Promise((ok) => setTimeout(ok, 3000));
        continue;
      }
      for (const m of r.data) {
        try {
          handle(m);
        } catch (e) {
          console.debug("[tuios-pip]", m.type, e);
        }
      }
    }
    polling = false;
  }

  function adopt(v) {
    const switched = video && video !== v && rec;
    video = v;
    hello(true);
    if (switched) start();
    poll();
  }

  document.addEventListener(
    "play",
    (e) => {
      if (e.target instanceof HTMLVideoElement) adopt(e.target);
    },
    true,
  );

  // keep the picker's playing/paused mark current
  document.addEventListener(
    "pause",
    (e) => {
      if (e.target === video) hello(false);
    },
    true,
  );

  // size is often unknown at play time, and changes with quality switches
  document.addEventListener(
    "resize",
    (e) => {
      if (e.target === video) hello(false);
    },
    true,
  );

  // autoplay often starts before this script loads, so no play event comes
  const already = [...document.querySelectorAll("video")].find((v) => !v.paused && !v.ended);
  if (already) adopt(already);

  // heartbeat so a restarted helper finds us again without stealing focus
  // from the tab that played last
  setInterval(() => {
    if (video) hello(false);
  }, 15000);

  addEventListener("pagehide", () => {
    if (video) send({ type: "bye" });
  });
})();
