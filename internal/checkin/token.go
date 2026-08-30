// Package checkin is the secondary innovation: attendance at the venue, and the
// automatic release of a court nobody turned up for.
//
// It is deliberately cheap. Everything expensive was already built:
//
//   - The QR token is a keyed hash of (facility, minute). Nothing is stored,
//     nothing is synced, and the venue display needs no network beyond the poll
//     that renders it.
//   - A no-show releases its window by changing the booking's STATUS. NO_SHOW is
//     outside no_double_book's predicate, so the court is bookable again the
//     instant the sweep commits — the same mechanism a cancellation uses, and the
//     same reason there is no is_available column to clear (non-negotiable #4).
//   - The freed window is offered to the queue through waitlist.Service.Promote,
//     the SAME statement a live cancel and the expiry sweeper claim through. There
//     is no second promotion implementation in this package, and there must not
//     be: two independent readers of the WAITING rows would eventually hand one
//     student two courts.
package checkin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"time"

	"github.com/google/uuid"
)

// Window is how long one minted token is the CURRENT token. The venue display
// re-renders on this cadence.
//
// Sixty seconds is the number IMPLEMENTATION.md §7 names, and it is a trade
// between two failure modes: a longer window lets a student photograph the code
// and check in from the hostel, a shorter one fails honest scans. Verification
// accepting the previous window as well turns that into ~2 minutes of tolerance
// for clock skew and slow scanning, which is the smaller of the two evils —
// a screenshot is worth at most two minutes, and a wrong rejection at the door
// costs a student their booking.
const Window = 60 * time.Second

// Minter mints and verifies venue check-in tokens.
//
// Stateless by construction. Two API replicas with the same secret agree without
// talking to each other, a replica that restarts loses nothing, and a token
// cannot be forged without the secret. Nothing here touches Postgres or Redis —
// which is what makes the venue display work on a laptop with no network at all.
type Minter struct {
	secret []byte
	now    func() time.Time
}

// NewMinter builds the token authority over CHECKIN_HMAC_SECRET.
//
// An EMPTY secret disables minting and verification rather than signing with a
// zero-length key: an unconfigured deployment must fail closed, not accept
// tokens anybody could compute. Callers should warn at boot; see httpx.NewRouter.
func NewMinter(secret string) *Minter {
	return &Minter{secret: []byte(secret), now: time.Now}
}

// WithClock overrides the clock. Used by tests to step across window boundaries
// without sleeping through them.
func (m *Minter) WithClock(now func() time.Time) *Minter {
	m.now = now
	return m
}

// Enabled reports whether a secret was configured.
func (m *Minter) Enabled() bool { return len(m.secret) > 0 }

// Mint returns the token for facilityID in the CURRENT window.
//
// Empty when no secret is configured — see NewMinter.
func (m *Minter) Mint(facilityID uuid.UUID) string {
	if !m.Enabled() {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(m.sign(facilityID, m.window()))
}

// Verify reports whether token is a live token for facilityID, accepting the
// CURRENT and the PREVIOUS window.
//
// The comparison is hmac.Equal and never ==. A byte-by-byte string comparison
// returns as soon as it finds a difference, which leaks how much of a guess was
// right and turns forging a token into a few thousand timed requests instead of
// 2^256 of work. Both candidates are computed before the result is combined, so
// a token that matches the previous window costs the same as one that matches
// the current one.
func (m *Minter) Verify(token string, facilityID uuid.UUID) bool {
	if !m.Enabled() || token == "" {
		return false
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		// Not even the right shape. Nothing to compare against.
		return false
	}

	w := m.window()
	current := hmac.Equal(raw, m.sign(facilityID, w))
	previous := hmac.Equal(raw, m.sign(facilityID, w-1))
	return current || previous
}

// ExpiresIn is how long the token Mint just returned stays CURRENT, so the venue
// display knows when to re-render. It stays VALID for one window beyond that.
func (m *Minter) ExpiresIn() time.Duration {
	elapsed := m.now().UTC().Sub(time.Unix(m.window()*int64(Window/time.Second), 0).UTC())
	return Window - elapsed
}

// window is floor(unix / 60): the minute this token belongs to.
func (m *Minter) window() int64 {
	return m.now().UTC().Unix() / int64(Window/time.Second)
}

// sign is HMAC-SHA256 over facility_id || window.
//
// The window number is inside the MAC rather than carried alongside it. A token
// that announced its own window would let a scanner replay yesterday's code by
// relabelling it; here the only way to produce the bytes for a given minute is
// to hold the secret during that minute.
func (m *Minter) sign(facilityID uuid.UUID, window int64) []byte {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write(facilityID[:])
	mac.Write([]byte{'.'})

	var buf [8]byte
	for i := 7; i >= 0; i-- {
		buf[i] = byte(window)
		window >>= 8
	}
	mac.Write(buf[:])

	return mac.Sum(nil)
}
