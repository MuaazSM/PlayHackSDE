package main

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReportPrintReflectsCheckedFailures(t *testing.T) {
	rep := &Report{
		N:           2,
		ByStatus:    map[int]int{http.StatusCreated: 1, http.StatusConflict: 1},
		ConfirmP99:  time.Millisecond,
		ConflictP99: ConflictP99Budget,
	}
	rep.Check(true)

	var out bytes.Buffer
	rep.Print(&out, true)
	require.Contains(t, out.String(), "FAIL")
	require.NotContains(t, out.String(), "PASS")
}

func TestReportCheckWithoutLatencyStillChecksTransportAndCorrectness(t *testing.T) {
	rep := &Report{
		N:        2,
		ByStatus: map[int]int{http.StatusCreated: 1, http.StatusConflict: 1},
		Errors:   1,
	}

	failures := rep.CheckWithoutLatency(true)
	require.Contains(t, failures, "1 transport errors (want 0)")
	require.Equal(t, failures, rep.Failures)
}
