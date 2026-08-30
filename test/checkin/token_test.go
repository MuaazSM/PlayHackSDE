package checkin_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iitg-playhack/sportsbook/internal/checkin"
	"github.com/stretchr/testify/require"
)

// windowBase is a unix time that lands exactly on a 60-second window boundary
// (1700000040 / 60 = 28333334, no remainder).
//
// Aligned on purpose. Starting mid-window would make "one window later" depend
// on how far into the current one the test began, and the boundary cases are
// precisely what these tests are about.
var windowBase = time.Unix(1_700_000_040, 0).UTC()

// atClock returns a minter pinned to a fixed instant.
func atClock(secret string, at time.Time) *checkin.Minter {
	return checkin.NewMinter(secret).WithClock(func() time.Time { return at })
}

// TestQRToken_ValidInCurrentWindow is the base case: the code on the wall right
// now opens the door right now.
func TestQRToken_ValidInCurrentWindow(t *testing.T) {
	facilityID := uuid.New()

	minted := atClock(testSecret, windowBase)
	token := minted.Mint(facilityID)
	require.NotEmpty(t, token)

	// The verifier is a DIFFERENT Minter with the same secret, which is the whole
	// point of the design: two API replicas agree without sharing state, and a
	// replica that restarted mid-evening still accepts the code on the wall.
	require.True(t, atClock(testSecret, windowBase).Verify(token, facilityID))

	// Still the same window most of a minute later.
	require.True(t, atClock(testSecret, windowBase.Add(59*time.Second)).Verify(token, facilityID))

	// A different secret is a different authority. This is the property that
	// makes the token unforgeable: knowing the facility id and the minute is not
	// enough, and both are public.
	require.False(t, atClock("another-secret", windowBase).Verify(token, facilityID))
}

// TestQRToken_ValidInPreviousWindow is the clock-skew and slow-scan allowance
// from §7: verification accepts the previous window, which is what turns a
// 60-second display refresh into roughly two minutes of real tolerance.
//
// Without it, a student who photographs the screen at 18:00:59 and finishes
// scanning at 18:01:01 is refused for being two seconds late — and a phone
// whose clock is thirty seconds out is refused all evening.
func TestQRToken_ValidInPreviousWindow(t *testing.T) {
	facilityID := uuid.New()
	token := atClock(testSecret, windowBase).Mint(facilityID)

	// One window later: this token is now the PREVIOUS window's, and is accepted.
	require.True(t, atClock(testSecret, windowBase.Add(60*time.Second)).Verify(token, facilityID),
		"a token one window old must still open the door")

	// The far edge of the allowance, one second before it lapses.
	require.True(t, atClock(testSecret, windowBase.Add(119*time.Second)).Verify(token, facilityID),
		"the tolerance runs to the end of the following window")
}

// TestQRToken_InvalidAfterTwoWindows closes the allowance. A screenshot is worth
// at most two minutes; after that the code on the phone is not the code on the
// wall, and holding a booking is no longer evidence of standing in front of it.
func TestQRToken_InvalidAfterTwoWindows(t *testing.T) {
	facilityID := uuid.New()
	token := atClock(testSecret, windowBase).Mint(facilityID)

	require.False(t, atClock(testSecret, windowBase.Add(120*time.Second)).Verify(token, facilityID),
		"two windows on, the token must be dead")
	require.False(t, atClock(testSecret, windowBase.Add(10*time.Minute)).Verify(token, facilityID))
	require.False(t, atClock(testSecret, windowBase.Add(24*time.Hour)).Verify(token, facilityID))

	// And it was never valid BEFORE it was minted, which matters because the
	// window number is inside the MAC: a token cannot be pre-computed and held.
	require.False(t, atClock(testSecret, windowBase.Add(-60*time.Second)).Verify(token, facilityID))
}

// TestQRToken_WrongFacilityRejected is what stops one screenshot checking a
// student into every venue on campus. The facility id is inside the MAC, so the
// tennis courts' code is not the gym's code even in the same minute.
func TestQRToken_WrongFacilityRejected(t *testing.T) {
	tennis, gym := uuid.New(), uuid.New()

	m := atClock(testSecret, windowBase)
	tennisToken := m.Mint(tennis)

	require.True(t, m.Verify(tennisToken, tennis))
	require.False(t, m.Verify(tennisToken, gym),
		"a token minted for one venue must not open another")

	// And the two venues' codes genuinely differ in the same window — if they did
	// not, the assertion above would be passing for the wrong reason.
	require.NotEqual(t, tennisToken, m.Mint(gym))
}

// TestQRToken_TamperedRejected walks the shapes a forged or corrupted code
// arrives in. None of them may be accepted, and none may panic — this input
// comes off a camera pointed at whatever somebody printed.
func TestQRToken_TamperedRejected(t *testing.T) {
	facilityID := uuid.New()
	m := atClock(testSecret, windowBase)
	token := m.Mint(facilityID)

	// Flip one character of the base64 payload. Every bit of the MAC is load
	// bearing; there is no slack to absorb an edit.
	flipped := []byte(token)
	if flipped[0] == 'A' {
		flipped[0] = 'B'
	} else {
		flipped[0] = 'A'
	}
	require.False(t, m.Verify(string(flipped), facilityID), "a single-character edit must be rejected")

	cases := map[string]string{
		"empty":            "",
		"truncated":        token[:len(token)-4],
		"extended":         token + "AAAA",
		"not base64":       "!!!not-base64!!!",
		"padded base64":    token + "==",
		"plausible length": strings.Repeat("A", len(token)),
		"the facility id":  facilityID.String(),
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			require.False(t, m.Verify(bad, facilityID))
		})
	}
}

// TestQRToken_EmptySecretFailsClosed pins the misconfiguration case.
//
// An unset CHECKIN_HMAC_SECRET must refuse everything rather than sign with a
// zero-length key — which would be a key every attacker also has. Failing closed
// makes the mistake visible at the door; failing open would make it invisible
// until the no-show numbers stopped meaning anything.
func TestQRToken_EmptySecretFailsClosed(t *testing.T) {
	facilityID := uuid.New()
	unset := atClock("", windowBase)

	require.False(t, unset.Enabled())
	require.Empty(t, unset.Mint(facilityID))

	// Not even a token minted with a configured secret gets in.
	genuine := atClock(testSecret, windowBase).Mint(facilityID)
	require.False(t, unset.Verify(genuine, facilityID))
	require.False(t, unset.Verify("", facilityID))
}

// TestQRToken_ExpiresInTracksTheWindow is what the venue display polls on: how
// long until this code stops being the current one.
func TestQRToken_ExpiresInTracksTheWindow(t *testing.T) {
	require.Equal(t, checkin.Window, atClock(testSecret, windowBase).ExpiresIn())
	require.Equal(t, 30*time.Second,
		atClock(testSecret, windowBase.Add(30*time.Second)).ExpiresIn())
	require.Equal(t, time.Second,
		atClock(testSecret, windowBase.Add(59*time.Second)).ExpiresIn())
}
