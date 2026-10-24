# tuios-pip

Picture-in-picture for tuios. Press cmd+p and whatever video is playing in Firefox or Chrome shows up in a floating tuios pane. It's the same tab, not a copy: pause, seek or skip in the pane and the real tab does it.

The extension grabs the playing `<video>` with `captureStream()`, records it in 50ms WebM slices and hands them to a small Go helper on 127.0.0.1. `tuios-pip-helper view` runs in a `tuios popup`, decodes the slices with ffmpeg and draws the frames with the kitty graphics protocol. No browser on the terminal side. Sound stays in the browser; the picture trails it by about 0.3s.

## Setup

Needs Go and ffmpeg (`brew install ffmpeg`). Build the helper:

```sh
go build -o bin/tuios-pip-helper ./helper
```

Load the extension from `extension/`:

- Chrome / Chromium / Helium: `chrome://extensions`, turn on developer mode, Load unpacked.
- Firefox / Zen: `about:debugging#/runtime/this-firefox`, Load Temporary Add-on, pick `manifest.json`. Release Firefox drops it on restart, so for a permanent install sign it as unlisted on addons.mozilla.org or use Developer Edition. Firefox also treats host permissions as opt-in: open the add-on in `about:addons`, Permissions, and allow `127.0.0.1`.

## Modes

`tuios-pip [popup|corner|pane]` toggles the viewer. A second press of any mode closes whichever one is open.

- `popup` (default): floating in the middle, full quality, sized to the video's shape.
- `corner`: tuios's own pip view in a corner, so it stays up while you work in other panes. tuios's pip view copies text only, so this mode draws with coloured half-blocks: lower res, but always there. The pane behind it lives on workspace 9 (`TUIOS_PIP_WORKSPACE`). Corner and size are tuios settings: `pip.corner` (bottom-right, bottom-left, top-right, top-left), `pip.width`, `pip.height`. Bigger is sharper. Click the corner view to jump to the pane and use the keys.
- `pane`: a normal tiled pane in the current layout, full quality.

There's no sidebar mode: tuios's rail only draws its own sections.

Bind keys in tuios (`~/.config/tuios/config.d/pip.toml`):

```toml
[[keybindings.command]]
name = "pip-toggle"
key = "prefix+alt+p"
type = "shell"
command = "'/path/to/tuios-pip/tuios-pip' popup"
description = "Video PiP: popup"

[[keybindings.command]]
name = "pip-corner"
key = "prefix+alt+o"
type = "shell"
command = "'/path/to/tuios-pip/tuios-pip' corner"
description = "Video PiP: corner"

[[keybindings.command]]
name = "pip-pane"
key = "prefix+alt+i"
type = "shell"
command = "'/path/to/tuios-pip/tuios-pip' pane"
description = "Video PiP: pane"

[keybindings.global]
"command:pip-toggle" = ["super+p"]
"command:pip-corner" = ["super+alt+p"]
"command:pip-pane" = ["super+ctrl+p"]
```

The script starts the helper if it isn't running, so there's nothing to keep alive yourself.

## Keys in the pane

space or k play/pause, left/right 5s, j/l 10s, n/p next/prev track (via `media-control`), q or esc close.

s opens a list of every tab with a video: up/down (or j/k) and enter to switch, esc to close. A tab you pick stays on screen even if another one starts playing, until you pick again or close it.

## Knobs

- `TUIOS_PIP_SIZE` popup width as % of the screen (default 60)
- `TUIOS_PIP_MODE` default mode when none is given (default popup)
- `TUIOS_PIP_WIDTH` decode width in pixels (default 640, or 240 in corner mode). Higher is sharper and heavier.
- `TUIOS_PIP_FPS` max frames drawn per second (default 30)
- `TUIOS_PIP_CELL` cell height / width, if the terminal doesn't report pixel sizes and the fit is off (default 2.0)

There's also a browser viewer at http://127.0.0.1:7420 if you'd rather watch it in a tab or terminal-browser.

## Limits

- DRM video (Netflix, Disney+, Prime) can't be captured. The browser blocks it.
- One tab is shown at a time: the one you picked, otherwise the one that started playing most recently.
- A paused video sends no frames, so the pane stays blank until it plays.
- After changing the extension, reload it (and the video tab), or the old content script keeps running.
- The extension talks to port 7420. `TUIOS_PIP_PORT` changes the helper's port, but then edit `BASE` in `extension/background.js` too.
