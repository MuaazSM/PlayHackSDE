package demo_test

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/iitg-playhack/sportsbook/test/testutil"
	"github.com/stretchr/testify/require"
)

// TestRaceDemoCLI_ExitsZeroAndPrintsSplit runs `cmd/racedemo` end to end, the
// way `make race-demo N=500` does.
//
// It is a separate process on purpose. The CLI is a product surface, not a
// script: a judge may run it, and the things that break a CLI — flag parsing, a
// missing DB_URL, an exit code that lies — are exactly the things a test calling
// run() in-process would never notice.
//
// n is small here because what is under test is the CLI, not the constraint;
// TestRaceDemo_OneWinnerAtN500 does the 500-way version.
func TestRaceDemoCLI_ExitsZeroAndPrintsSplit(t *testing.T) {
	pg := testutil.Postgres(t)

	const n = 20
	start, _ := futureSlot()

	out, err := runCLI(t, pg.DSN,
		"-n", strconv.Itoa(n),
		"-facility", "tennis-court-1",
		"-at", start.Format(time.RFC3339),
	)
	require.NoErrorf(t, err, "racedemo must exit 0 when the invariant holds:\n%s", out)

	// The one line a presenter and a judge both read.
	require.Contains(t, out, "confirmed=1 conflicts=19 other=0 db_count=1")

	// And the proof stated as the query that produced it, so nobody has to take
	// the number on trust.
	require.Contains(t, out, "SELECT count(*) FROM bookings")
	require.Contains(t, out, "winner")

	t.Logf("racedemo output:\n%s", out)
}

// TestRaceDemoCLI_ResetMakesItRerunnable is the stage requirement: fire, fire
// again, twice, without restarting anything.
func TestRaceDemoCLI_ResetMakesItRerunnable(t *testing.T) {
	pg := testutil.Postgres(t)

	start, _ := futureSlot()
	args := []string{"-n", "10", "-facility", "tennis-court-1", "-at", start.Format(time.RFC3339)}

	for i := 1; i <= 3; i++ {
		out, err := runCLI(t, pg.DSN, args...)
		require.NoErrorf(t, err, "run %d failed:\n%s", i, out)
		require.Containsf(t, out, "confirmed=1 conflicts=9 other=0 db_count=1",
			"run %d did not reset cleanly:\n%s", i, out)
	}

	// -reset=false is the "fire again — still 1" beat: nobody wins, and the
	// count is unchanged. That must still be an exit-0 outcome, because nothing
	// about it violates the invariant.
	out, err := runCLI(t, pg.DSN, append(args, "-reset=false")...)
	require.NoErrorf(t, err, "firing again without a reset is a valid outcome:\n%s", out)
	require.Contains(t, out, "confirmed=0 conflicts=10 other=0 db_count=1")
}

// runCLI builds and runs cmd/racedemo against the given database.
func runCLI(t *testing.T, dsn string, args ...string) (string, error) {
	t.Helper()

	root := repoRoot(t)
	bin := filepath.Join(t.TempDir(), "racedemo")

	build := exec.Command("go", "build", "-o", bin, "./cmd/racedemo")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building cmd/racedemo: %v\n%s", err, out)
	}

	cmd := exec.Command(bin, args...)
	cmd.Dir = root
	// Only the database. No Redis, no API, no network beyond localhost — if the
	// CLI needed anything else, it would fail here, which is the point.
	cmd.Env = append(os.Environ(), "DB_URL="+dsn, "DB_REPLICA_URL=", "TZ_DISPLAY=Asia/Kolkata")

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// packageImports lists every import path in the .go files of a package,
// relative to the repo root.
func packageImports(t *testing.T, pkgDir string) []string {
	t.Helper()

	dir := filepath.Join(repoRoot(t), filepath.FromSlash(pkgDir))
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, nil, parser.ImportsOnly)
	require.NoError(t, err)

	var out []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				require.NoError(t, err)
				out = append(out, path)
			}
		}
	}
	require.NotEmpty(t, out, "expected %s to import something", pkgDir)
	return out
}

// repoRoot resolves the module root from the migrations directory testutil
// already locates, so nothing here depends on the working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := testutil.MigrationsDir()
	require.NoError(t, err)
	return filepath.Dir(dir)
}
