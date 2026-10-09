package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

const (
	// maxHookRead leaves room for JSON escaping and ANSI codes that are
	// stripped before the 64 KiB body limit applies.
	maxHookRead     = 2 * proto.MaxBody
	maxIdemKey      = 256
	httpShutdownMax = 2 * time.Second
)

// serveHTTP serves webhooks on hl. The returned function shuts the HTTP
// server down (in-flight requests get httpShutdownMax) and waits for it.
func (s *Server) serveHTTP(ctx context.Context, hl net.Listener) (stop func()) {
	hs := s.newHTTPServer(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		hs.Serve(hl) // returns once hl is closed
	}()
	return func() {
		sctx, cancel := context.WithTimeout(context.Background(), httpShutdownMax)
		defer cancel()
		if hs.Shutdown(sctx) != nil {
			hs.Close()
		}
		<-done
	}
}

func (s *Server) newHTTPServer(ctx context.Context) *http.Server {
	return &http.Server{
		Handler:           s.hookHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    16 << 10,
		ConnState:         s.hk.conns.state,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
}

type hookError struct {
	Error string `json:"error"`
	Msg   string `json:"msg,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func hookFail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, hookError{Error: code, Msg: msg})
}

// hookHandler serves POST /hooks/<token> (PROTOCOL.md §12). Everything else,
// and unknown or deleted tokens, get the same 404.
func (s *Server) hookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Per source, before anything costs a database read.
		if ok, wait := s.hk.ips.allow(r.RemoteAddr); !ok {
			retryAfter(w, wait)
			hookFail(w, http.StatusTooManyRequests, proto.ErrRateLimited, "slow down")
			return
		}
		token, ok := strings.CutPrefix(r.URL.Path, proto.HookPath)
		if !ok || r.Method != http.MethodPost || !validHookToken(token) {
			hookFail(w, http.StatusNotFound, proto.ErrNotFound, "")
			return
		}
		ctx := r.Context()
		h, err := s.hookByToken(ctx, token)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				s.log.Error("webhook lookup", "err", err)
				hookFail(w, http.StatusInternalServerError, proto.ErrInternal, "")
				return
			}
			hookFail(w, http.StatusNotFound, proto.ErrNotFound, "")
			return
		}
		id := h.id
		if ok, wait := s.allowHook(id); !ok {
			retryAfter(w, wait)
			hookFail(w, http.StatusTooManyRequests, proto.ErrRateLimited, "slow down")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if len(key) > maxIdemKey {
			hookFail(w, http.StatusBadRequest, proto.ErrBadRequest, "Idempotency-Key too long")
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookRead))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				hookFail(w, http.StatusRequestEntityTooLarge, proto.ErrTooLarge, "")
				return
			}
			hookFail(w, http.StatusBadRequest, proto.ErrBadRequest, "reading body")
			return
		}
		text, msg := hookText(r.Header.Get("Content-Type"), raw)
		if msg != "" {
			hookFail(w, http.StatusBadRequest, proto.ErrBadRequest, msg)
			return
		}
		nonce := make([]byte, 16)
		if key != "" {
			nonce = hookNonce(h.tokenHash, key)
		} else {
			rand.Read(nonce)
		}
		res, err := s.send(ctx, h.bot, proto.SendArgs{Chan: h.ch, Body: text, Nonce: nonce}, &h)
		if err != nil {
			var pe *proto.Error
			if !errors.As(err, &pe) {
				s.log.Error("webhook send", "hook", id, "err", err)
				hookFail(w, http.StatusInternalServerError, proto.ErrInternal, "")
				return
			}
			switch pe.Code {
			case proto.ErrTooLarge:
				hookFail(w, http.StatusRequestEntityTooLarge, pe.Code, "")
			case proto.ErrBadRequest:
				hookFail(w, http.StatusBadRequest, pe.Code, pe.Msg)
			case proto.ErrConflict:
				hookFail(w, http.StatusConflict, pe.Code, "Idempotency-Key reused for a different message")
			case proto.ErrRateLimited:
				retryAfter(w, time.Second)
				hookFail(w, http.StatusTooManyRequests, pe.Code, "slow down")
			default: // forbidden/not_found: the bot lost access
				hookFail(w, http.StatusNotFound, proto.ErrNotFound, "")
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]uint64{"msg": res.(proto.SendRes).Msg})
	})
}

func retryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(d.Seconds())))))
}

// hookText extracts the message text from a request body; a non-empty msg
// explains why it's unacceptable. Size limits are left to send.
func hookText(contentType string, raw []byte) (text, msg string) {
	body := string(raw)
	if mt, _, _ := mime.ParseMediaType(contentType); mt == "application/json" {
		var p struct {
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(raw, &p); err != nil || p.Text == nil {
			return "", `body must be {"text": "..."}`
		}
		body = *p.Text
	}
	if !utf8.ValidString(body) {
		return "", "text must be UTF-8"
	}
	body = strings.TrimRight(sanitize.StripANSI(body), " \t\n")
	if strings.TrimSpace(body) == "" {
		return "", "empty message"
	}
	return body, ""
}

// hookNonce makes retries with the same Idempotency-Key idempotent sends.
// It's keyed by the hook's token hash (hex SHA-256 of the token), which
// is unique forever, unlike a row id.
func hookNonce(tokenHash, key string) []byte {
	raw, _ := hex.DecodeString(tokenHash)
	h := sha256.New()
	h.Write(raw)
	h.Write([]byte(key))
	return h.Sum(nil)[:16]
}
