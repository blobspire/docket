// Package ulid makes ULIDs: 26-character, time-sortable, unique IDs
// (48-bit millisecond timestamp + 80 random bits, Crockford base32).
package ulid

import (
	"crypto/rand"
	"math/big"
	"strings"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns a ULID for time t.
func New(t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	if _, err := rand.Read(b[6:]); err != nil {
		panic("ulid: no randomness: " + err.Error())
	}
	return encode(b)
}

func encode(b [16]byte) string {
	n := new(big.Int).SetBytes(b[:])
	base := big.NewInt(32)
	mod := new(big.Int)
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		n.DivMod(n, base, mod)
		out[i] = alphabet[mod.Int64()]
	}
	return string(out)
}

// ItemID returns a short, human-friendly item ID such as "dk-7f3k9q2m", taken
// from the random part of a fresh ULID. 40 random bits is plenty for one
// person's queue, and minting it where an item is filed means the ID is the
// same everywhere without asking the server.
func ItemID() string {
	return "dk-" + strings.ToLower(New(time.Now())[10:18])
}
