package fmtx

import (
	"math"
	"testing"
)

func TestSignedTokensMinInt64(t *testing.T) {
	const want = "-9223372036.85 B"
	if got := SignedTokens(math.MinInt64); got != want {
		t.Fatalf("SignedTokens(math.MinInt64) = %q, want %q", got, want)
	}
}
