package identity

import (
	"encoding/binary"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
)

// CtxAgentEndorse signs an AgentEndorse (PROTOCOL.md §15.1).
const CtxAgentEndorse = "tc-agent-endorse-v1\x00"

// AgentEndorse is an owner's SSH key vouching that AgentSSH is the key of
// User's agent. Unlike a KeyEndorse it makes AgentSSH an agent key only:
// devices it certifies are the agent's, and it can't endorse keys or revoke
// devices. Time is Unix ms.
type AgentEndorse struct {
	ServerID e2ecrypto.ServerID
	User     uint64
	AgentSSH []byte // ssh wire-encoded
	Time     int64
}

type agentEndorseB struct {
	V        uint32 `msgpack:"v"`
	ServerID []byte `msgpack:"server_id"`
	User     uint64 `msgpack:"user"`
	AgentSSH []byte `msgpack:"agent_ssh"`
	Time     int64  `msgpack:"time"`
}

// NewAgentEndorse signs, with one of user's own keys, that agentKey is the
// key of user's agent.
func NewAgentEndorse(signer ssh.Signer, serverID e2ecrypto.ServerID, user uint64, agentKey ssh.PublicKey, now time.Time) (Signed, error) {
	if !allowedKey[agentKey.Type()] {
		return Signed{}, fmt.Errorf("key type %s not accepted", agentKey.Type())
	}
	return sign(signer, CtxAgentEndorse, agentEndorseB{
		V: signedVersion, ServerID: serverID[:], User: user, AgentSSH: agentKey.Marshal(), Time: now.UnixMilli(),
	})
}

// VerifyAgentEndorse checks s as signed by endorserPub (wire-encoded). The
// caller must already trust endorserPub as a (non-agent) key of e.User.
func (s Signed) VerifyAgentEndorse(serverID e2ecrypto.ServerID, endorserPub []byte) (AgentEndorse, error) {
	if err := s.verifySSH(endorserPub, CtxAgentEndorse); err != nil {
		return AgentEndorse{}, err
	}
	e, err := decodeAgentEndorse(s.B)
	if err != nil {
		return AgentEndorse{}, err
	}
	if e.ServerID != serverID {
		return AgentEndorse{}, ErrWrongServer
	}
	return e, nil
}

// PeekAgentEndorse decodes an unverified agent endorsement.
func PeekAgentEndorse(s Signed) (AgentEndorse, error) {
	if len(s.B)+len(s.Sig) > MaxSignedSize {
		return AgentEndorse{}, e2ecrypto.ErrTooLarge
	}
	return decodeAgentEndorse(s.B)
}

func decodeAgentEndorse(b []byte) (AgentEndorse, error) {
	var w agentEndorseB
	if err := decodeB(b, &w, &w.V); err != nil {
		return AgentEndorse{}, err
	}
	e := AgentEndorse{User: w.User, AgentSSH: w.AgentSSH, Time: w.Time}
	if !copyFixed(e.ServerID[:], w.ServerID) {
		return AgentEndorse{}, e2ecrypto.ErrMalformed
	}
	if pk, err := ssh.ParsePublicKey(w.AgentSSH); err != nil || !allowedKey[pk.Type()] {
		return AgentEndorse{}, e2ecrypto.ErrMalformed
	}
	return e, nil
}

// AllowedKeyType reports whether SSH keys of type t are accepted (§4).
func AllowedKeyType(t string) bool { return allowedKey[t] }

// CtxAgentKey signs an agent key's proof of possession (PROTOCOL.md §15.2
// agent.key).
const CtxAgentKey = "tc-agent-key-v1\x00"

// agentKeyPayload is server_id || owner (u64 big-endian) || pubkey: fixed
// widths before the key, so no two inputs share a payload.
func agentKeyPayload(serverID e2ecrypto.ServerID, owner uint64, pubkey []byte) []byte {
	b := make([]byte, 0, len(serverID)+8+len(pubkey))
	b = append(b, serverID[:]...)
	b = binary.BigEndian.AppendUint64(b, owner)
	return append(b, pubkey...)
}

// SignAgentKey proves, with the agent's own key, that whoever registers it
// as owner's agent key holds it: else anyone could register someone else's
// public key (from github.com/<user>.keys, say) and lock them out of tc.
func SignAgentKey(agentKey ssh.Signer, serverID e2ecrypto.ServerID, owner uint64) ([]byte, error) {
	return SignContext(agentKey, CtxAgentKey, agentKeyPayload(serverID, owner, agentKey.PublicKey().Marshal()))
}

// VerifyAgentKey checks a SignAgentKey proof for pubkey (wire-encoded).
func VerifyAgentKey(pubkey, proof []byte, serverID e2ecrypto.ServerID, owner uint64) error {
	_, err := VerifyContext(pubkey, proof, CtxAgentKey, agentKeyPayload(serverID, owner, pubkey))
	return err
}
