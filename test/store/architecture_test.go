package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sqlstatePattern matches a five-character SQLSTATE literal: two digits or
// uppercase letters for the class, three for the subclass.
var sqlstatePattern = regexp.MustCompile(`"[0-9A-Z]{5}"`)

// TestNoSQLSTATEOutsidePgerr enforces the rule that would otherwise decay the
// moment someone is in a hurry: exactly one file may inspect a SQLSTATE.
//
// Without this, a handler eventually grows an `if pgErr.Code == "23505"`, the
// meaning of that code drifts from the one in pgerr.go, and an idempotent replay
// starts returning a 409. Documented rules do not survive a hackathon; a failing
// test does.
func TestNoSQLSTATEOutsidePgerr(t *testing.T) {
	root := repoRoot(t)
	allowed := filepath.Join(root, "internal", "store", "pgerr.go")

	var offenders []string

	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if path == allowed {
				return nil
			}

			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			for i, line := range strings.Split(string(body), "\n") {
				code := strings.TrimSpace(line)
				// Comments may name a code; only real string literals count.
				if strings.HasPrefix(code, "//") {
					continue
				}
				if m := sqlstatePattern.FindString(line); m != "" {
					rel, _ := filepath.Rel(root, path)
					offenders = append(offenders, rel+":"+strconv.Itoa(i+1)+" "+m)
				}
			}
			return nil
		})
		require.NoError(t, err)
	}

	require.Empty(t, offenders,
		"SQLSTATE literals found outside internal/store/pgerr.go — route these through "+
			"store.Classify instead:\n  %s", strings.Join(offenders, "\n  "))
}

// TestNoInlineSQLOutsideQueries keeps the write-path SQL in .sql files, where it
// can be read as SQL. Handlers and services must not assemble statements inline.
func TestNoInlineSQLOutsideQueries(t *testing.T) {
	root := repoRoot(t)

	// Packages that legitimately hold SQL: the query loader (embeds it) and the
	// seed (fixture data, not the write path).
	allowedDirs := []string{
		filepath.Join(root, "internal", "store", "queries"),
		filepath.Join(root, "internal", "seed"),
	}

	var offenders []string
	err := filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		for _, dir := range allowedDirs {
			if strings.HasPrefix(path, dir) {
				return nil
			}
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			upper := strings.ToUpper(trimmed)
			if strings.Contains(upper, "INSERT INTO ") || strings.Contains(upper, "UPDATE ") && strings.Contains(upper, " SET ") {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+":"+strconv.Itoa(i+1))
			}
		}
		return nil
	})
	require.NoError(t, err)

	require.Empty(t, offenders,
		"inline SQL found outside internal/store/queries — move it to a .sql file:\n  %s",
		strings.Join(offenders, "\n  "))
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	// test/store/architecture_test.go -> repo root
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}
