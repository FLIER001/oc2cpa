package main

import (
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"strings"
	"sync"
	"time"
)

// sessionRegistryDefault reuses one session per auth holder (the auth id), the
// same TTL the original server used per API-key user.
var sessionRegistryDefault = newSessionRegistry()

// The upstream merges sessions by x-opencode-session and only accepts session
// ids that look like OpenCode ids: "ses_" + 12 hex chars + 14 base62 chars.
// The hex part is not random: it must equal the high 48 bits of
// ~(timestamp*0x1000 + counter) — the upstream derives it from its own
// descending() id generator and validates the shape.
const (
	ocBase62Chars   = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	ocSessionTTLSec = 30 * 60
)

func ocSessionID(timestamp int64) string {
	// value = ~(timestamp * 0x1000 + counter) kept to the low 48 bits, matching
	// the upstream descending() algorithm exactly.
	value := uint64(timestamp)*0x1000 + 1
	low48 := (^value) & 0xFFFF_FFFF_FFFF
	var timePart strings.Builder
	for index := 0; index < 6; index++ {
		byteAt := byte(low48 >> (40 - 8*index))
		timePart.WriteString(hex.EncodeToString([]byte{byteAt}))
	}
	random := make([]byte, 14)
	if _, err := rand.Read(random); err != nil {
		// Degraded fallback that keeps the required shape; never fails the request.
		var fallback [14]byte
		bigRand, err := rand.Int(rand.Reader, big.NewInt(1<<62))
		if err == nil {
			randState := bigRand.Uint64()
			for i := 0; i < 14; i++ {
				fallback[i] = byte(randState >> (8 * (i % 8)))
				randState = randState*6364136223846793005 + 1
			}
			random = fallback[:]
		}
	}
	var randomPart strings.Builder
	for _, b := range random {
		randomPart.WriteByte(ocBase62Chars[int(b)%62])
	}
	return "ses_" + timePart.String() + randomPart.String()
}

// sessionRegistry reuses one session per auth holder for up to 30 minutes, the
// way the original server reused one session per API-key user.
type sessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]sessionEntry
	now      func() time.Time
}

type sessionEntry struct {
	id    string
	start time.Time
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{sessions: make(map[string]sessionEntry), now: time.Now}
}

func (s *sessionRegistry) get(holder string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if entry, ok := s.sessions[holder]; ok && now.Sub(entry.start) < time.Duration(ocSessionTTLSec)*time.Second {
		return entry.id
	}
	id := ocSessionID(now.UnixMilli())
	s.sessions[holder] = sessionEntry{id: id, start: now}
	return id
}
