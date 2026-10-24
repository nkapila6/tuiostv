package main

// view: the terminal viewer. It joins the helper like the web viewer does,
// pipes the WebM slices through ffmpeg and draws the frames with the kitty
// graphics protocol, so nothing on this side needs a browser.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

type source struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Host   string `json:"host"`
	Paused bool   `json:"paused"`
}

type decoder struct {
	gen  int
	cmd  *exec.Cmd
	feed chan []byte
}

type tview struct {
	base string
	id   string
	out  *bufio.Writer

	mu        sync.Mutex // everything below, and all terminal writes
	cols      int
	rows      int
	cellRatio float64 // cell height / cell width in pixels
	title     string
	t, dur    float64
	paused    bool
	sources   []source
	active    string
	picker    bool
	sel       int
	note      string // shown instead of video: nothing playing, replaced...
	dec       *decoder
	gen       int
	frame     []byte // latest PNG from ffmpeg
	fw, fh    int
	drawn     bool // frame already on screen
	placed    [4]int
	// blocks draws with coloured half-block characters instead of kitty
	// graphics: low-res, but it's plain text, so tuios's pip corner view
	// (which copies text cells only) can show it
	blocks bool
	regW   int // area to draw in, in cells; 0 means the whole pane
	regH   int
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil && n > 0 {
		return n
	}
	return def
}

func runView(port string, args []string) {
	v := &tview{
		base: "http://127.0.0.1:" + port,
		id:   strconv.FormatInt(time.Now().UnixNano(), 36),
		out:  bufio.NewWriterSize(os.Stdout, 1<<20),
		note: "Nothing playing. Play a video in Firefox or Chrome.",
	}
	for _, a := range args {
		if a == "--blocks" {
			v.blocks = true
		}
	}
	// TUIOS_PIP_REGION=WxH limits drawing to the top-left corner, which is
	// the part tuios's pip view copies
	fmt.Sscanf(os.Getenv("TUIOS_PIP_REGION"), "%dx%d", &v.regW, &v.regH)
	restore := rawMode()
	quit := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(quit) }) }

	v.mu.Lock()
	v.resize()
	io.WriteString(v.out, "\x1b[?1049h\x1b[?25l\x1b[2J")
	v.redraw()
	v.mu.Unlock()

	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range sig {
			if s != syscall.SIGWINCH {
				if os.Getenv("TUIOS_PIP_DEBUG") != "" {
					fmt.Fprintf(os.Stderr, "signal: %v\n", s)
				}
				stop()
				return
			}
			v.mu.Lock()
			v.resize()
			v.redraw()
			v.mu.Unlock()
		}
	}()

	// join before the first poll, or the helper answers 410 to a stranger
	v.post(map[string]any{"type": "join"})
	go v.keys(stop)
	go v.drawLoop(quit)
	go v.pollLoop(stop)

	<-quit
	v.post(map[string]any{"type": "bye"})
	v.mu.Lock()
	v.stopDecoder()
	io.WriteString(v.out, "\x1b_Ga=d,d=A,q=2\x1b\\\x1b[?25h\x1b[?1049l")
	v.out.Flush()
	v.mu.Unlock()
	restore()
}

// rawMode turns off line buffering and echo with stty, which keeps this
// stdlib only. It returns the undo.
func rawMode() func() {
	get := exec.Command("stty", "-g")
	get.Stdin = os.Stdin
	saved, err := get.Output()
	set := exec.Command("stty", "raw", "-echo")
	set.Stdin = os.Stdin
	set.Run()
	return func() {
		if err != nil {
			return
		}
		r := exec.Command("stty", strings.TrimSpace(string(saved)))
		r.Stdin = os.Stdin
		r.Run()
	}
}

type winsize struct{ Row, Col, Xpixel, Ypixel uint16 }

// caller holds v.mu
func (v *tview) resize() {
	var ws winsize
	syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	v.cols, v.rows = int(ws.Col), int(ws.Row)
	if v.cols == 0 || v.rows == 0 {
		v.cols, v.rows = 80, 24
	}
	v.cellRatio = 2.0
	if f, err := strconv.ParseFloat(os.Getenv("TUIOS_PIP_CELL"), 64); err == nil && f > 0 {
		v.cellRatio = f
	} else if ws.Xpixel > 0 && ws.Ypixel > 0 {
		v.cellRatio = (float64(ws.Ypixel) / float64(v.rows)) / (float64(ws.Xpixel) / float64(v.cols))
	}
}

func (v *tview) post(msg map[string]any) {
	b, _ := json.Marshal(msg)
	req, _ := http.NewRequest("POST", v.base+"/msg?from=viewer&id="+v.id, bytes.NewReader(b))
	req.Header.Set("X-Pip", "1")
	req.Header.Set("Content-Type", "application/json")
	if r, err := http.DefaultClient.Do(req); err == nil {
		r.Body.Close()
	}
}

func (v *tview) pollLoop(stop func()) {
	for {
		req, _ := http.NewRequest("GET", v.base+"/poll?role=viewer&id="+v.id, nil)
		req.Header.Set("X-Pip", "1")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			v.setNote("Lost the tuios-pip helper. Retrying...")
			time.Sleep(2 * time.Second)
			v.post(map[string]any{"type": "join"})
			continue
		}
		if r.StatusCode == http.StatusGone {
			r.Body.Close()
			v.setNote("Another viewer took over. Closing.")
			time.Sleep(1500 * time.Millisecond)
			stop()
			return
		}
		var msgs []json.RawMessage
		json.NewDecoder(r.Body).Decode(&msgs)
		r.Body.Close()
		for _, m := range msgs {
			v.handle(m)
		}
	}
}

func (v *tview) handle(raw json.RawMessage) {
	var m struct {
		Type   string   `json:"type"`
		Gen    int      `json:"gen"`
		Data   string   `json:"data"`
		Title  string   `json:"title"`
		T      float64  `json:"t"`
		Dur    *float64 `json:"dur"`
		Paused bool     `json:"paused"`
		List   []source `json:"list"`
		Active string   `json:"active"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	switch m.Type {
	case "begin":
		v.startDecoder(m.Gen)
	case "chunk":
		if v.dec == nil || m.Gen != v.dec.gen {
			return
		}
		if b, err := base64.StdEncoding.DecodeString(m.Data); err == nil {
			select {
			case v.dec.feed <- b:
			default:
				// ffmpeg fell far behind; a gap beats a growing delay
			}
		}
	case "state":
		was := v.paused
		v.title, v.t, v.paused = m.Title, m.T, m.Paused
		v.dur = math.Inf(1)
		if m.Dur != nil {
			v.dur = *m.Dur
		}
		// a paused video sends no frames, so say why the area is empty
		if v.frame == nil && was != v.paused {
			v.redraw()
		}
		v.drawHUD()
	case "sources":
		v.sources, v.active = m.List, m.Active
		if v.sel >= len(v.sources) {
			v.sel = max(0, len(v.sources)-1)
		}
		if v.picker {
			v.drawPicker()
		}
		v.drawHUD()
	case "reset":
		v.stopDecoder()
		v.clearVideo()
	case "idle":
		v.stopDecoder()
		v.note = "Nothing playing. Play a video in Firefox or Chrome."
		v.redraw()
	}
}

func (v *tview) setNote(s string) {
	v.mu.Lock()
	v.note = s
	v.redraw()
	v.mu.Unlock()
}

// caller holds v.mu
func (v *tview) startDecoder(gen int) {
	v.stopDecoder()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		ffmpeg = "/opt/homebrew/bin/ffmpeg"
	}
	width := envInt("TUIOS_PIP_WIDTH", 640)
	if v.blocks {
		// a few source pixels per half-block keep the averaging smooth
		width = envInt("TUIOS_PIP_WIDTH", 240)
	}
	cmd := exec.Command(ffmpeg, "-loglevel", "error",
		// small probe + low_delay: first frame in ~0.1s instead of ~1.6s.
		// Adding -fflags nobuffer breaks VP8 decoding of these slices.
		"-probesize", "32768", "-flags", "low_delay",
		"-f", "webm", "-i", "pipe:0", "-an",
		// no fps filter: it holds frames back. drawLoop caps the rate and
		// only ever draws the newest frame.
		"-vf", fmt.Sprintf("scale=%d:-2", width),
		"-f", "image2pipe", "-c:v", "png", "-compression_level", "1", "pipe:1")
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		v.note = "ffmpeg is needed to show video: brew install ffmpeg"
		v.redraw()
		return
	}
	d := &decoder{gen: gen, cmd: cmd, feed: make(chan []byte, 512)}
	v.dec, v.gen, v.note = d, gen, ""
	// TUIOS_PIP_DUMP=file keeps a copy of what ffmpeg gets, for debugging
	var dump io.Writer = io.Discard
	if p := os.Getenv("TUIOS_PIP_DUMP"); p != "" {
		if f, err := os.Create(p); err == nil {
			dump = f
		}
	}
	go func() {
		for b := range d.feed {
			dump.Write(b)
			if _, err := in.Write(b); err != nil {
				break
			}
		}
		in.Close()
	}()
	go v.readFrames(d, out)
}

// caller holds v.mu
func (v *tview) stopDecoder() {
	if v.dec == nil {
		return
	}
	close(v.dec.feed)
	v.dec.cmd.Process.Kill()
	go v.dec.cmd.Wait()
	v.dec = nil
	v.frame = nil
}

// readFrames splits ffmpeg's PNG stream into frames and keeps only the
// newest; drawLoop picks it up, so a slow terminal drops frames instead of
// falling behind.
func (v *tview) readFrames(d *decoder, out io.Reader) {
	br := bufio.NewReaderSize(out, 1<<20)
	for {
		var buf bytes.Buffer
		sig := make([]byte, 8)
		if _, err := io.ReadFull(br, sig); err != nil {
			return
		}
		buf.Write(sig)
		for {
			hdr := make([]byte, 8)
			if _, err := io.ReadFull(br, hdr); err != nil {
				return
			}
			buf.Write(hdr)
			n := binary.BigEndian.Uint32(hdr[:4])
			if _, err := io.CopyN(&buf, br, int64(n)+4); err != nil {
				return
			}
			if string(hdr[4:8]) == "IEND" {
				break
			}
		}
		png := buf.Bytes()
		w := int(binary.BigEndian.Uint32(png[16:20]))
		h := int(binary.BigEndian.Uint32(png[20:24]))
		v.mu.Lock()
		if v.dec == d {
			v.frame, v.fw, v.fh, v.drawn = png, w, h, false
		}
		v.mu.Unlock()
	}
}

func (v *tview) drawLoop(quit chan struct{}) {
	tick := time.NewTicker(time.Second / time.Duration(envInt("TUIOS_PIP_FPS", 30)))
	defer tick.Stop()
	for {
		select {
		case <-quit:
			return
		case <-tick.C:
			v.mu.Lock()
			if v.frame != nil && !v.drawn && !v.picker {
				v.drawFrame()
			}
			v.mu.Unlock()
		}
	}
}

// fit returns where the video goes, in cells: col, row, width, height.
// The bottom two rows belong to the HUD.
func (v *tview) fit() [4]int {
	area := max(1, v.rows-2)
	if v.fw == 0 || v.fh == 0 {
		return [4]int{0, 0, v.cols, area}
	}
	aspect := float64(v.fw) / float64(v.fh)
	c := v.cols
	r := int(math.Round(float64(c) / aspect / v.cellRatio))
	if r > area {
		r = area
		c = int(math.Round(float64(r) * v.cellRatio * aspect))
	}
	return [4]int{(v.cols - c) / 2, (area - r) / 2, max(1, c), max(1, r)}
}

// region is the drawing area in cells
func (v *tview) region() (int, int) {
	w, h := v.cols, v.rows
	if !v.blocks {
		h = max(1, v.rows-2)
	}
	if v.regW > 0 {
		w = min(w, v.regW)
	}
	if v.regH > 0 {
		h = min(h, v.regH)
	}
	return w, h
}

// caller holds v.mu
func (v *tview) drawFrame() {
	if v.blocks {
		v.drawBlocks()
		return
	}
	p := v.fit()
	if p != v.placed {
		// layout moved: wipe what the old placement covered
		v.clearVideo()
		v.placed = p
	}
	data := base64.StdEncoding.EncodeToString(v.frame)
	fmt.Fprintf(v.out, "\x1b[%d;%dH", p[1]+1, p[0]+1)
	for i := 0; i < len(data); i += 4096 {
		end := min(i+4096, len(data))
		more := 0
		if end < len(data) {
			more = 1
		}
		if i == 0 {
			fmt.Fprintf(v.out, "\x1b_Ga=T,f=100,i=1,p=1,c=%d,r=%d,q=2,C=1,m=%d;%s\x1b\\", p[2], p[3], more, data[i:end])
		} else {
			fmt.Fprintf(v.out, "\x1b_Gm=%d;%s\x1b\\", more, data[i:end])
		}
	}
	v.drawn = true
	v.out.Flush()
}

// drawBlocks paints the frame with "▀": the foreground colour is the top
// pixel of a cell and the background the bottom one.
// caller holds v.mu
func (v *tview) drawBlocks() {
	v.drawn = true
	img, err := png.Decode(bytes.NewReader(v.frame))
	if err != nil {
		return
	}
	W, H := v.region()
	rows := H * 2
	// a half-block pixel is cellRatio/2 times as tall as it is wide
	pa := v.cellRatio / 2
	aspect := float64(v.fw) / float64(v.fh)
	w := W
	h := int(math.Round(float64(w) / (aspect * pa)))
	if h > rows {
		h = rows
		w = int(math.Round(float64(h) * aspect * pa))
	}
	w, h = max(1, min(w, W)), max(1, min(h, rows))
	x0, y0 := (W-w)/2, ((rows-h)/2)&^1
	if p := [4]int{x0, y0, w, h}; p != v.placed {
		v.clearVideo()
		v.drawn = true
		v.placed = p
	}

	b := img.Bounds()
	pixel := func(px, py int) (uint32, uint32, uint32) {
		if px < x0 || px >= x0+w || py < y0 || py >= y0+h {
			return 0, 0, 0
		}
		// average the source pixels this one covers
		sx0 := b.Min.X + (px-x0)*b.Dx()/w
		sx1 := max(sx0+1, b.Min.X+(px-x0+1)*b.Dx()/w)
		sy0 := b.Min.Y + (py-y0)*b.Dy()/h
		sy1 := max(sy0+1, b.Min.Y+(py-y0+1)*b.Dy()/h)
		var r, g, bl, n uint32
		for y := sy0; y < sy1; y++ {
			for x := sx0; x < sx1; x++ {
				cr, cg, cb, _ := img.At(x, y).RGBA()
				r, g, bl, n = r+cr>>8, g+cg>>8, bl+cb>>8, n+1
			}
		}
		return r / n, g / n, bl / n
	}

	var sb strings.Builder
	for cy := 0; cy < H; cy++ {
		fmt.Fprintf(&sb, "\x1b[%d;1H", cy+1)
		last := ""
		for cx := 0; cx < W; cx++ {
			tr, tg, tb := pixel(cx, cy*2)
			br, bg, bb := pixel(cx, cy*2+1)
			c := fmt.Sprintf("\x1b[38;2;%d;%d;%d;48;2;%d;%d;%dm", tr, tg, tb, br, bg, bb)
			if c != last {
				sb.WriteString(c)
				last = c
			}
			sb.WriteString("▀")
		}
		sb.WriteString("\x1b[0m")
	}
	io.WriteString(v.out, sb.String())
	v.out.Flush()
}

// caller holds v.mu
func (v *tview) clearVideo() {
	if !v.blocks {
		io.WriteString(v.out, "\x1b_Ga=d,d=A,q=2\x1b\\")
	}
	_, h := v.region()
	for r := 1; r <= h; r++ {
		fmt.Fprintf(v.out, "\x1b[%d;1H\x1b[2K", r)
	}
	v.placed = [4]int{}
	v.drawn = false
	v.out.Flush()
}

// caller holds v.mu
func (v *tview) redraw() {
	v.clearVideo()
	if v.picker {
		v.drawPicker()
	} else if msg := v.message(); msg != "" {
		w, h := v.region()
		msg = clip(msg, w)
		fmt.Fprintf(v.out, "\x1b[%d;%dH\x1b[2m%s\x1b[0m", max(1, (h+1)/2), max(1, (w-utf8.RuneCountInString(msg))/2+1), msg)
	}
	v.drawHUD()
}

// message is the text shown where the video would be, if any
func (v *tview) message() string {
	if v.dec == nil {
		if v.blocks {
			return "nothing playing"
		}
		return v.note
	}
	if v.frame == nil && v.paused {
		if v.blocks {
			return "paused"
		}
		return "Paused. Press play in the browser, or space here."
	}
	return ""
}

func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:max(0, n-1)]) + "…"
}

func clock(s float64) string {
	if math.IsInf(s, 0) || math.IsNaN(s) {
		return "live"
	}
	n := int(s)
	if n >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", n/3600, n%3600/60, n%60)
	}
	return fmt.Sprintf("%d:%02d", n/60, n%60)
}

// caller holds v.mu
func (v *tview) drawHUD() {
	if v.rows < 3 || v.blocks {
		return
	}
	mark := "▶"
	if v.paused {
		mark = "⏸"
	}
	extra := ""
	if len(v.sources) > 1 {
		extra = fmt.Sprintf("  s: %d videos", len(v.sources))
	}
	times := clock(v.t) + " / " + clock(v.dur)
	if math.IsInf(v.dur, 0) {
		times = "live"
	}
	barW := v.cols - utf8.RuneCountInString(times) - utf8.RuneCountInString(extra) - 6
	bar := ""
	if barW > 4 {
		done := 0
		if v.dur > 0 && !math.IsInf(v.dur, 0) {
			done = int(float64(barW) * math.Min(1, v.t/v.dur))
		}
		bar = strings.Repeat("━", done) + "\x1b[2m" + strings.Repeat("─", barW-done) + "\x1b[0m"
	}
	fmt.Fprintf(v.out, "\x1b[%d;1H\x1b[2K %s", v.rows-1, clip(v.title, v.cols-2))
	fmt.Fprintf(v.out, "\x1b[%d;1H\x1b[2K %s %s %s\x1b[2m%s\x1b[0m", v.rows, mark, bar, times, extra)
	v.out.Flush()
}

// caller holds v.mu
func (v *tview) drawPicker() {
	io.WriteString(v.out, "\x1b_Ga=d,d=A,q=2\x1b\\")
	for r := 1; r <= max(1, v.rows-2); r++ {
		fmt.Fprintf(v.out, "\x1b[%d;1H\x1b[2K", r)
	}
	v.placed, v.drawn = [4]int{}, false
	fmt.Fprintf(v.out, "\x1b[1;2H\x1b[1mVideos in your browser\x1b[0m")
	if len(v.sources) == 0 {
		fmt.Fprintf(v.out, "\x1b[3;2H\x1b[2mNo tabs with a video yet.\x1b[0m")
	}
	for i, s := range v.sources {
		row := 3 + i
		if row > v.rows-4 {
			break
		}
		state := "playing"
		if s.Paused {
			state = "paused"
		}
		meta := strings.Join(nonEmpty(s.Host, state), " · ")
		dot := " "
		if s.ID == v.active {
			dot = "●"
		}
		name := clip(s.Title, v.cols-utf8.RuneCountInString(meta)-8)
		style := ""
		if i == v.sel {
			style = "\x1b[7m"
		}
		pad := v.cols - 6 - utf8.RuneCountInString(name) - utf8.RuneCountInString(meta)
		fmt.Fprintf(v.out, "\x1b[%d;2H%s %s %s%s\x1b[2m%s\x1b[0m", row, style, dot, name, strings.Repeat(" ", max(1, pad)), meta)
	}
	fmt.Fprintf(v.out, "\x1b[%d;2H\x1b[2m↑↓ choose · enter show · esc close\x1b[0m", max(1, v.rows-3))
	v.out.Flush()
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// keys reads the terminal. Plain bytes and the usual CSI arrows; a lone ESC
// shows up as a single-byte read.
func (v *tview) keys(stop func()) {
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			if os.Getenv("TUIOS_PIP_DEBUG") != "" {
				fmt.Fprintf(os.Stderr, "stdin: %v\n", err)
			}
			stop()
			return
		}
		in := string(buf[:n])
		if os.Getenv("TUIOS_PIP_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "keys %q\n", in)
		}
		for len(in) > 0 {
			key := in[:1]
			switch {
			case strings.HasPrefix(in, "\x1b[") && len(in) >= 3:
				key = in[:3]
			case strings.HasPrefix(in, "\x1bO") && len(in) >= 3:
				key = "\x1b[" + in[2:3]
			}
			in = in[len(key):]
			if v.key(key, stop) {
				return
			}
		}
	}
}

// key handles one key; true means quit
func (v *tview) key(k string, stop func()) bool {
	const up, down, right, left = "\x1b[A", "\x1b[B", "\x1b[C", "\x1b[D"
	v.mu.Lock()
	if v.picker {
		switch k {
		case up, "k":
			v.sel = max(0, v.sel-1)
		case down, "j":
			v.sel = min(len(v.sources)-1, v.sel+1)
		case "\r", "\n":
			v.picker = false
			if v.sel < len(v.sources) && v.sources[v.sel].ID != v.active {
				id := v.sources[v.sel].ID
				v.redraw()
				v.mu.Unlock()
				v.post(map[string]any{"type": "pick", "source": id})
				return false
			}
			v.redraw()
			v.mu.Unlock()
			return false
		case "\x1b", "s", "q":
			v.picker = false
			v.redraw()
			v.mu.Unlock()
			return false
		default:
			v.mu.Unlock()
			return false
		}
		v.drawPicker()
		v.mu.Unlock()
		return false
	}
	v.mu.Unlock()

	switch k {
	case "q", "\x1b", "\x03":
		stop()
		return true
	case "s":
		v.mu.Lock()
		v.picker = true
		v.sel = 0
		for i, s := range v.sources {
			if s.ID == v.active {
				v.sel = i
			}
		}
		v.drawPicker()
		v.mu.Unlock()
	case " ", "k":
		v.post(map[string]any{"type": "cmd", "action": "toggle"})
	case left:
		v.post(map[string]any{"type": "cmd", "action": "seek", "by": -5})
	case right:
		v.post(map[string]any{"type": "cmd", "action": "seek", "by": 5})
	case "j":
		v.post(map[string]any{"type": "cmd", "action": "seek", "by": -10})
	case "l":
		v.post(map[string]any{"type": "cmd", "action": "seek", "by": 10})
	case "n":
		v.post(map[string]any{"type": "next"})
	case "p":
		v.post(map[string]any{"type": "prev"})
	}
	return false
}
