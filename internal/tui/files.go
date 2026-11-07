package tui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/media"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Files and images (PROTOCOL.md §16; docs/TUI.md "Files"). A message's files
// show as cards under its text; an image with a thumbnail also gets a
// block-art preview drawn in ▀ cells, which is plain text to the renderer,
// so it fits every frame and works in tmux and herdr. In select mode s saves
// a message's files to the downloads folder, o saves and opens one; nothing
// is ever opened by itself, and only a short list of passive types opens.

const (
	kSelSave = "s" // save the selected message's files
	kSelOpen = "o" // save and open its file (o still unfolds agent steps first)

	// downloadTimeout bounds one save: 25 MiB over a slow link.
	downloadTimeout = 10 * time.Minute

	// thumbCols and thumbRows bound the block-art preview.
	thumbCols = 40
	thumbRows = 10
)

// hidesBody: the text is only the "📎 name" stand-in for clients without
// files, which the cards say better.
func hidesBody(x client.Message) bool {
	return len(x.Files) > 0 && x.Body == client.FallbackBody(x.Files)
}

// showsFiles: the message's files may be shown (its text may be).
func showsFiles(x client.Message) bool {
	return len(x.Files) > 0 && !x.Deleted && x.Sec != client.SecWaiting && x.Sec != client.SecInvalid
}

// previewOK: the terminal shows enough colors for a block-art preview.
func (m *Model) previewOK() bool {
	return m.st.color && (m.st.prof == termenv.TrueColor || m.st.prof == termenv.ANSI256)
}

// drawFiles draws a card per file (and an image's preview) under the text.
func (m *Model) drawFiles(g *grower, x client.Message, bw int, at func(int) int) {
	th := m.st.th
	for i, f := range x.Files {
		if g.full() {
			return
		}
		icon := m.icon(icFile)
		if media.IsImage(f.Mime) || len(f.Thumb) > 0 {
			icon = m.icon(icImage)
		}
		name := sanitize.Line(media.SafeName(f.Name))
		meta := " · " + media.Size(f.Size)
		if f.W > 0 && f.H > 0 && media.IsImage(f.Mime) {
			meta = fmt.Sprintf(" · %d×%d", f.W, f.H) + meta
		}
		if x.Local < 0 && x.Pending && x.UpTotal > 0 && i == 0 {
			meta += fmt.Sprintf(" · uploading %d%%", min(99, x.UpSent*100/x.UpTotal))
		}
		lead := icon + " "
		room := bw - strWidth(lead) - strWidth(meta)
		if room < 4 {
			meta, room = "", bw-strWidth(lead)
		}
		name = truncate(name, max(room, 1))
		w := strWidth(lead + name + meta)
		y := g.row()
		xx := g.c.put(at(w), y, lead, fgc(th.info), "")
		act := ""
		if x.ID != 0 && client.Downloadable(x, i) {
			act = "file|" + strconv.Itoa(i)
		}
		xx = g.c.put(xx, y, name, fgb(th.fg), act)
		g.c.put(xx, y, meta, fgc(th.dim), "")
		// The card comes first, so a short pane still shows what the file is.
		if len(f.Thumb) > 0 && m.previewOK() && !x.Pending && x.Failed == "" {
			m.drawPreview(g, f, bw, at)
		}
	}
}

// drawPreview draws f's thumbnail in ▀ cells: each cell two pixels, the
// upper one its foreground, the lower one its background.
func (m *Model) drawPreview(g *grower, f proto.File, bw int, at func(int) int) {
	img, err := media.DecodeThumb(f.Thumb)
	if err != nil {
		return
	}
	b := img.Bounds()
	iw, ih := b.Dx(), b.Dy()
	if iw <= 0 || ih <= 0 {
		return
	}
	cols := min(thumbCols, bw-1, iw)
	rows := max(1, (cols*ih/iw+1)/2)
	if rows > thumbRows {
		rows = thumbRows
		cols = max(1, min(cols, 2*rows*iw/ih))
	}
	if cols < 4 {
		return
	}
	small := media.Fit(img, cols, 2*rows)
	sb := small.Bounds()
	x0 := at(cols)
	for cy := 0; cy < (sb.Dy()+1)/2 && !g.full(); cy++ {
		y := g.row()
		for cx := 0; cx < sb.Dx(); cx++ {
			top := small.RGBAAt(cx, 2*cy)
			bot := top
			if 2*cy+1 < sb.Dy() {
				bot = small.RGBAAt(cx, 2*cy+1)
			}
			fg := rgb(uint32(top.R)<<16 | uint32(top.G)<<8 | uint32(top.B))
			bg := rgb(uint32(bot.R)<<16 | uint32(bot.G)<<8 | uint32(bot.B))
			g.c.put(x0+cx, y, "▀", fgc(fg).on(bg), "")
		}
	}
}

// fileKeysFor is the select footer's file keys for x ("" when none).
func fileKeysFor(x client.Message) []string {
	if !showsFiles(x) || x.ID == 0 || !client.Downloadable(x, 0) {
		return nil
	}
	if len(x.Files) == 1 {
		return []string{"s/o save/open"}
	}
	return []string{"s save all"}
}

// savable is how many files s would save from x (0: none it can).
func savable(x client.Message) int {
	if len(fileKeysFor(x)) == 0 {
		return 0
	}
	return len(x.Files)
}

type savedMsg struct {
	id    uint64
	paths []string
	open  bool
	err   error
}

// saveFiles saves sel's file only (-1: all of them; open: its only file,
// then opens it if it is a kind tc opens).
func (m *Model) saveFiles(sel client.Message, only int, open bool) tea.Cmd {
	switch {
	case len(sel.Files) > 0 && sel.ID == 0 && sel.Failed != "":
		m.setInfo("not sent — r retries")
		return nil
	case len(sel.Files) > 0 && sel.ID == 0:
		m.setInfo("still uploading")
		return nil
	case !showsFiles(sel):
		m.setInfo("no file in this message")
		return nil
	case !client.Downloadable(sel, 0):
		m.setInfo("this message isn't verified: its files can't be saved")
		return nil
	case m.saving[sel.ID]:
		m.setInfo("already saving that")
		return nil
	}
	if open && len(sel.Files) > 1 {
		m.setInfo(fmt.Sprintf("o opens a message's only file; this one has %d — s saves them all", len(sel.Files)))
		return nil
	}
	dir := m.opt.DownloadDir
	if dir == "" {
		dir = client.DownloadsDir()
	}
	be, ctx := m.c, m.ctx
	if m.saving == nil {
		m.saving = map[uint64]bool{}
	}
	id, idx := sel.ID, []int{}
	for i := range sel.Files {
		if only < 0 || i == only {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return nil
	}
	m.saving[id] = true
	if len(idx) == 1 {
		m.setHint("saving " + sanitize.Line(media.SafeName(sel.Files[idx[0]].Name)) + "…")
	} else {
		m.setHint(fmt.Sprintf("saving %d files…", len(idx)))
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
		defer cancel()
		out := savedMsg{id: id, open: open}
		for _, i := range idx {
			p, err := be.Download(ctx, id, i, dir, nil)
			if err != nil {
				out.err = err
				break
			}
			out.paths = append(out.paths, p)
		}
		return out
	}
}

func (m *Model) gotSaved(msg savedMsg) tea.Cmd {
	delete(m.saving, msg.id)
	if msg.err != nil {
		if len(msg.paths) > 0 {
			m.setErr("save", fmt.Errorf("saved %d, then: %w", len(msg.paths), msg.err))
		} else {
			m.setErr("save", msg.err)
		}
		return nil
	}
	if len(msg.paths) == 0 {
		return nil
	}
	shown := sanitize.Line(tildePath(msg.paths[0]))
	if len(msg.paths) > 1 {
		shown = fmt.Sprintf("%d files to %s", len(msg.paths), sanitize.Line(tildePath(filepath.Dir(msg.paths[0]))))
	}
	if !msg.open {
		m.setInfo("saved " + shown)
		return nil
	}
	p := msg.paths[0]
	if why := unopenable(p); why != "" {
		m.setInfo("saved " + shown + " · " + why)
		return nil
	}
	open := m.opt.OpenFile
	if open == nil || m.opt.SSH || m.opt.NoBrowser {
		why := " · not opened: no display"
		if m.opt.SSH {
			why = " · not opened over SSH"
		}
		m.setInfo("saved " + shown + why)
		return nil
	}
	ctx := m.ctx
	return func() tea.Msg {
		if err := open(ctx, p); err != nil {
			return errMsg{"open", err}
		}
		return infoMsg("opened " + shown)
	}
}

// openable maps the extensions tc hands to the desktop's opener to the
// type the file's bytes must sniff as: passive documents and media only.
// The opener picks an app by extension, so both must agree.
var openable = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".bmp": "image/bmp", ".pdf": "application/pdf", ".txt": "text/plain", ".md": "text/plain", ".csv": "text/plain",
	".log": "text/plain", ".json": "text/plain", ".mp3": "audio/mpeg", ".wav": "audio/wave", ".mp4": "video/mp4",
}

// unopenable says why tc won't open the saved file at p ("" when it will).
func unopenable(p string) string {
	ext := strings.ToLower(filepath.Ext(p))
	want, ok := openable[ext]
	if !ok {
		return "not opened (" + safeExt(ext) + ")"
	}
	f, err := os.Open(p)
	if err != nil {
		return "couldn't read it back"
	}
	defer f.Close()
	head := make([]byte, media.SniffLen)
	n, _ := f.Read(head)
	got := media.Sniff(head[:n], "")
	if strings.HasPrefix(want, "text/") && got == "text/plain" || got == want {
		return ""
	}
	if want == "video/mp4" && strings.HasPrefix(got, "video/") || want == "audio/wave" && strings.HasPrefix(got, "audio/") {
		return ""
	}
	return "not opened: not really " + safeExt(ext)
}

// safeExt is ext for a notice ("no extension" when there is none), at most 10 bytes.
func safeExt(ext string) string {
	ext = sanitize.Line(ext)
	if ext == "" || ext == "." {
		return "no extension"
	}
	return truncate(ext, 10)
}

// defaultOpenFile hands a saved file to the desktop's opener as one
// argument, with no shell in between.
func defaultOpenFile(_ context.Context, p string) error {
	if !filepath.IsAbs(p) {
		return errors.New("not an absolute path")
	}
	name := "xdg-open"
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		return errors.New("opening files isn't supported here")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%s not found", name)
	}
	cmd := exec.Command(path, p)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// ---- sending ----

// uploadCommand is /upload path [caption]: the path may be quoted; without
// quotes, the longest leading run of words that names a file is the path.
func (m *Model) uploadCommand(rest string) (tea.Cmd, error) {
	f := m.focus()
	if !f.open() {
		return nil, errors.New("no channel open")
	}
	if strings.TrimSpace(rest) == "" {
		return nil, errors.New("usage: /upload path [caption] (or drag a file into the input)")
	}
	path, caption, ok := m.splitUploadArgs(rest)
	if !ok {
		return nil, fmt.Errorf("no such file: %s", sanitize.Line(rest))
	}
	if secretPath(path) {
		return nil, fmt.Errorf("%s looks like a key or credentials file; tc won't send it", sanitize.Line(filepath.Base(path)))
	}
	if big := m.tooBig([]string{path}); big != "" {
		return nil, errors.New(big)
	}
	return m.sendFiles(f.ch, f.root, caption, path), nil
}

// splitUploadArgs finds the file at the start of s.
func (m *Model) splitUploadArgs(s string) (path, caption string, ok bool) {
	s = strings.TrimSpace(s)
	if s != "" && (s[0] == '"' || s[0] == '\'') {
		if end := strings.IndexByte(s[1:], s[0]); end >= 0 {
			if p, ok := m.filePath(s[1 : end+1]); ok {
				return p, strings.TrimSpace(s[end+2:]), true
			}
		}
		return "", "", false
	}
	words := strings.Fields(s)
	for n := len(words); n > 0; n-- {
		if p, ok := m.filePath(strings.Join(words[:n], " ")); ok {
			return p, strings.Join(words[n:], " "), true
		}
	}
	return "", "", false
}

// filePath resolves s (~/, relative to SnippetDir, a file:// URL,
// backslash-escaped spaces) to an existing regular file.
func (m *Model) filePath(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "file://") {
		u, err := url.Parse(s)
		if err != nil || (u.Host != "" && u.Host != "localhost") {
			return "", false
		}
		s = u.Path
	}
	if strings.Contains(s, `\`) {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
			b.WriteByte(s[i])
		}
		s = b.String()
	}
	if s == "" || !utf8.ValidString(s) || strings.ContainsAny(s, "\n\x00") {
		return "", false
	}
	if strings.HasPrefix(s, "~/") {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		s = filepath.Join(h, s[2:])
	}
	if !filepath.IsAbs(s) {
		base := m.opt.SnippetDir
		if base == "" {
			base, _ = os.Getwd()
		}
		s = filepath.Join(base, s)
	}
	st, err := os.Stat(s)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return s, true
}

type uploadQueuedMsg struct {
	name string
	err  error
}

// sendFiles queues paths into ch (a thread: root) in a command: sealing a
// big file takes a moment.
func (m *Model) sendFiles(ch, root uint64, caption string, paths ...string) tea.Cmd {
	be := m.c
	name := filepath.Base(paths[0])
	if len(paths) > 1 {
		name = fmt.Sprintf("%d files", len(paths))
	}
	m.setInfo("preparing " + sanitize.Line(name) + "…")
	return func() tea.Msg {
		_, err := be.SendFiles(ch, root, caption, paths...)
		return uploadQueuedMsg{name: name, err: err}
	}
}

func (m *Model) gotUploadQueued(msg uploadQueuedMsg) {
	if msg.err != nil {
		m.setErr("upload", errors.New(sanitize.Line(msg.err.Error())))
		return
	}
	m.setInfo("sending " + sanitize.Line(msg.name))
}

// ---- paste offers: a dragged-in file, a paste too big for a message ----

// offerAttach arms Tab after a bracketed paste that is a path to a file
// (what a terminal types when a file is dragged in) or is over the message
// limit. It reports whether it made an offer.
func (m *Model) offerAttach(before, raw string) bool {
	after := m.input.Value()
	// A command's argument, but not a pasted absolute path.
	if strings.HasPrefix(before, "/") && !strings.HasPrefix(before, "//") {
		return false
	}
	if m.editing != 0 || m.c.MaxUpload() == 0 {
		return false
	}
	n := utf8.RuneCountInString(after) - utf8.RuneCountInString(before)
	end := m.cursorOffset()
	start := end - n
	rs := []rune(after)
	if n <= 0 || start < 0 || end > len(rs) {
		return false
	}
	ins := string(rs[start:end])
	w := &m.wr
	switch {
	case len(raw) > proto.MaxBody:
		if int64(len(raw)) > m.c.MaxUpload() {
			return false
		}
		m.offerHint(m.kn.of(fmt.Sprintf("⇥ send as file: paste.txt, %s (too long for a message)", media.Size(uint64(len(raw))))))
		w.attachText = raw // as pasted: the input turned tabs into spaces
	case !strings.Contains(strings.TrimSpace(ins), "\n"):
		paths := m.droppedPaths(ins)
		if len(paths) == 0 {
			return false
		}
		label := filepath.Base(paths[0])
		if len(paths) > 1 {
			label = fmt.Sprintf("%d files", len(paths))
		}
		if big := m.tooBig(paths); big != "" {
			m.setInfo(big)
			return false
		}
		m.offerHint(m.kn.of("⇥ send as file: " + sanitize.Line(label) + " · or keep typing"))
		w.attachPaths = paths
	default:
		return false
	}
	w.attachVal, w.attachFrom, w.attachTo = after, start, end
	return true
}

// tooBig says which of paths is over the server's limit ("" when none).
func (m *Model) tooBig(paths []string) string {
	limit := m.c.MaxUpload()
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.Size() > limit {
			return fmt.Sprintf("%s is %s; the server takes files up to %s", sanitize.Line(filepath.Base(p)),
				media.Size(uint64(st.Size())), media.Size(uint64(limit)))
		}
	}
	return ""
}

// droppedPaths reads s as one file path or several (space-separated, with
// spaces inside names escaped or quoted, as terminals type dropped files).
func (m *Model) droppedPaths(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64<<10 {
		return nil
	}
	if p, ok := m.droppedPath(unquote(s)); ok {
		return []string{p}
	}
	var out []string
	for _, part := range splitShellWords(s) {
		p, ok := m.droppedPath(part)
		if !ok {
			return nil
		}
		out = append(out, p)
	}
	if len(out) > proto.MaxFiles {
		return nil
	}
	return out
}

// droppedPath is filePath for what a terminal types when a file is dropped:
// an absolute, ~/ or file:// path (a pasted word like "Makefile" is text),
// and never a secret (secretPath).
func (m *Model) droppedPath(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "/") && !strings.HasPrefix(t, "~/") && !strings.HasPrefix(t, "file://") {
		return "", false
	}
	p, ok := m.filePath(t)
	if !ok || secretPath(p) {
		return "", false
	}
	return p, true
}

// secretPath: a file that holds keys or credentials, which tc never offers
// to send and /upload refuses (SSH and GPG keys, tc's own home, .env).
func secretPath(p string) bool {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	base := strings.ToLower(filepath.Base(p))
	if strings.HasPrefix(base, "id_") || strings.HasPrefix(base, ".env") || strings.HasSuffix(base, ".pem") ||
		strings.HasSuffix(base, ".key") || base == "credentials" || base == ".netrc" {
		return true
	}
	dirs := []string{".ssh", ".gnupg", ".aws", filepath.Join(".config", "tc")}
	h, _ := os.UserHomeDir()
	for _, d := range dirs {
		if h != "" && strings.HasPrefix(p, filepath.Join(h, d)+string(filepath.Separator)) {
			return true
		}
	}
	if th := os.Getenv("TC_HOME"); th != "" {
		if a, err := filepath.Abs(th); err == nil && strings.HasPrefix(p, a+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// splitShellWords splits on unescaped, unquoted spaces; quotes go, escapes stay
// (filePath removes them).
func splitShellWords(s string) []string {
	var out []string
	var b strings.Builder
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0 && c == q:
			q = 0
		case q != 0:
			b.WriteByte(c)
		case c == '\'' || c == '"':
			q = c
		case c == '\\' && i+1 < len(s):
			b.WriteByte(c)
			i++
			b.WriteByte(s[i])
		case c == ' ' || c == '\t':
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteByte(c)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func (m *Model) attachArmed() bool {
	return m.offerLive() && m.wr.attachVal != "" && m.input.Value() == m.wr.attachVal
}

// attachPaste takes the offered paste out of the input and sends it as a file.
func (m *Model) attachPaste() tea.Cmd {
	w := m.wr
	rs := []rune(w.attachVal)
	rest := string(rs[:w.attachFrom]) + string(rs[w.attachTo:])
	m.dropWriteHints()
	m.input.SetHeight(m.inputMax()) // see layoutInput
	m.input.SetValue(rest)
	m.layoutInput()
	f := m.focus()
	if !f.open() {
		m.setInfo("no channel open")
		return nil
	}
	if w.attachText != "" {
		return m.sendPasteFile(f.ch, f.root, w.attachText)
	}
	return m.sendFiles(f.ch, f.root, "", w.attachPaths...)
}

// sendPasteFile sends text as paste.txt. SendFiles seals a copy at once, so
// the temporary file goes right after.
func (m *Model) sendPasteFile(ch, root uint64, text string) tea.Cmd {
	be := m.c
	m.setInfo("preparing paste.txt…")
	return func() tea.Msg {
		dir, err := os.MkdirTemp("", "tc-paste-")
		if err != nil {
			return uploadQueuedMsg{err: err}
		}
		defer os.RemoveAll(dir)
		p := filepath.Join(dir, "paste.txt")
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			return uploadQueuedMsg{err: err}
		}
		_, err = be.SendFiles(ch, root, "", p)
		return uploadQueuedMsg{name: "paste.txt", err: err}
	}
}
