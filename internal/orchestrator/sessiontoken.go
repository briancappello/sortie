package orchestrator

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"sync"

	"github.com/sortie-ai/sortie/internal/redact"
)

// sessionTokenBytes is the entropy of a session token.
const sessionTokenBytes = 32

// sessionTokenPrefix starts every session token. Masking a cut text
// drops a trailing fragment that begins a registered value, so a fixed
// first byte that text rarely ends with keeps live tokens from trimming
// unrelated truncated text.
const sessionTokenPrefix = "_"

// mintSessionToken returns a fresh random session token: the prefix
// followed by 32 random bytes, base64url encoded without padding.
func mintSessionToken() string {
	buf := make([]byte, sessionTokenBytes)
	_, _ = rand.Read(buf) // crypto/rand.Read never returns an error
	return sessionTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
}

// SessionTokenIndex maps each live session token to the issue whose
// running session it was minted for. The event loop adds and removes
// entries with the running entry they belong to; the HTTP server reads
// it from its own goroutines. Tokens are keyed by their SHA-256 digest,
// so a lookup's timing reveals nothing about a token's bytes.
type SessionTokenIndex struct {
	mu     sync.RWMutex
	byHash map[[sha256.Size]byte]sessionTokenOwner
}

type sessionTokenOwner struct {
	issueID    string
	identifier string
}

// NewSessionTokenIndex returns an empty index.
func NewSessionTokenIndex() *SessionTokenIndex {
	return &SessionTokenIndex{byHash: make(map[[sha256.Size]byte]sessionTokenOwner)}
}

// Lookup returns the issue identifier of the running session token was
// minted for, and false when no running session holds it. A nil index
// holds no token.
func (x *SessionTokenIndex) Lookup(token string) (identifier string, ok bool) {
	if x == nil || token == "" {
		return "", false
	}
	x.mu.RLock()
	defer x.mu.RUnlock()
	owner, ok := x.byHash[sha256.Sum256([]byte(token))]
	return owner.identifier, ok
}

// register records token for the issue and registers it for masking.
func (x *SessionTokenIndex) register(token, issueID, identifier string) {
	if x == nil || token == "" {
		return
	}
	redact.Add("session token", token)
	x.mu.Lock()
	defer x.mu.Unlock()
	x.byHash[sha256.Sum256([]byte(token))] = sessionTokenOwner{issueID: issueID, identifier: identifier}
}

// revoke forgets token and withdraws its mask, so it stops working the
// moment its run ends.
func (x *SessionTokenIndex) revoke(token string) {
	if x == nil || token == "" {
		return
	}
	x.mu.Lock()
	delete(x.byHash, sha256.Sum256([]byte(token)))
	x.mu.Unlock()
	redact.Remove(token)
}
