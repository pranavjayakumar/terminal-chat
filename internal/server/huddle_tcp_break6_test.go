package server

import (
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// TestStreamVoiceStuckOnChatFrame_Break6: a huddle participant on the TCP
// fallback stops reading but keeps sending voice, so it never goes idle.
// On a real socket the write that blocks is chat or a PING, never voice
// (voice is skipped while the socket is backlogged), so the stuck check
// must look at any write, not only VOICE ones. (net.Pipe has no kernel
// backlog to watch: the time alone decides.)
func TestStreamVoiceStuckOnChatFrame_Break6(t *testing.T) {
	sess, hs, _ := pipeSession(t) // the peer never reads
	from, to := participants(hs, sess)
	go sess.writer()
	payload, _ := proto.Marshal(proto.Ping{T: 1})
	sess.push(outFrame{t: proto.TPing, payload: payload})
	deadline := time.Now().Add(voiceStuck + 500*time.Millisecond)
	for time.Now().Before(deadline) {
		sess.pushVoice(voiceItem(from, to, 1)) // voice keeps arriving
		time.Sleep(20 * time.Millisecond)
	}
	sess.checkVoiceStuck(time.Now()) // what readLoop's 5 s ticker does
	sess.mu.Lock()
	closed := sess.closed
	sess.mu.Unlock()
	if !closed {
		t.Fatalf("writer blocked %v on a peer that stopped reading, session still open",
			time.Since(time.Unix(0, sess.writing.Load())).Round(time.Second))
	}
}
