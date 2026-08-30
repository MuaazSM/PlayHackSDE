package store_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/iitg-playhack/sportsbook/internal/store/queries"
	"github.com/stretchr/testify/require"
)

// TestQueryLoader_MissingFileFails is the reason the loader exists: a typo in a
// query name must be a boot failure, not a runtime surprise on the one request
// that needs it.
//
// The load function is exercised directly against an in-memory FS, because the
// real package would have panicked at init if it were broken — which is exactly
// the behaviour under test.
func TestQueryLoader_MissingFileFails(t *testing.T) {
	t.Run("missing required query is an error", func(t *testing.T) {
		fsys := fstest.MapFS{
			"booking_insert_exclusive.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
			// capacity_take.sql deliberately absent
		}

		_, err := queries.LoadForTest(fsys, []string{"booking_insert_exclusive", "capacity_take"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "capacity_take", "the error must name the missing query")
	})

	t.Run("a misnamed file does not satisfy the requirement", func(t *testing.T) {
		fsys := fstest.MapFS{
			"capacity_takee.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		}

		_, err := queries.LoadForTest(fsys, []string{"capacity_take"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "capacity_take")
	})

	t.Run("a comment-only file counts as empty", func(t *testing.T) {
		fsys := fstest.MapFS{
			"capacity_take.sql": &fstest.MapFile{Data: []byte("-- TODO: write this\n-- really\n")},
		}

		_, err := queries.LoadForTest(fsys, []string{"capacity_take"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty")
	})

	t.Run("a complete set loads", func(t *testing.T) {
		fsys := fstest.MapFS{
			"capacity_take.sql": &fstest.MapFile{Data: []byte("-- doc\nSELECT 1;")},
		}

		loaded, err := queries.LoadForTest(fsys, []string{"capacity_take"})
		require.NoError(t, err)
		require.Contains(t, loaded["capacity_take"], "SELECT 1;")
	})
}

// TestQueryLoader_RealQueriesArePresent proves the embedded set actually loaded.
// If any .sql file were missing this package would not have initialised at all.
func TestQueryLoader_RealQueriesArePresent(t *testing.T) {
	names := queries.Names()
	require.Contains(t, names, queries.BookingInsertExclusive)
	require.Contains(t, names, queries.CapacityTake)
	require.Contains(t, names, queries.CapacityRelease)
	require.Contains(t, names, queries.WaitlistHeadForUpdate)

	// Mechanism A must be a bare INSERT. If a SELECT ever appears here, the
	// read-then-write gap is back.
	insert := queries.Get(queries.BookingInsertExclusive)
	require.Contains(t, insert, "INSERT INTO bookings")
	require.Contains(t, insert, "'[)'", "ranges must be half-open or adjacent slots collide")
	require.NotContains(t, strings.ToUpper(stripSQLComments(insert)), "SELECT",
		"the exclusive insert must not read occupancy first")

	require.Panics(t, func() { queries.Get("no_such_query") },
		"an unknown query name is a programming error, not a runtime branch")
}

func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
