package crontab

import (
	"testing"
	"time"
)

func TestParseAndNextUseFiveFieldUTCExpression(t *testing.T) {
	normalized, _, err := Parse("  15   8 * * 1-5 ")
	if err != nil {
		t.Fatal(err)
	}
	if normalized != "15 8 * * 1-5" {
		t.Fatalf("normalized = %q", normalized)
	}

	next, err := Next(normalized, time.Date(2026, time.July, 29, 9, 0, 0, 0, time.FixedZone("CST", 8*60*60)))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, time.July, 29, 8, 15, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
}

func TestParseRejectsNonCrontabForms(t *testing.T) {
	for _, expression := range []string{"", "*/30 * * * * *", "@every 5m", "61 * * * *"} {
		if _, _, err := Parse(expression); err == nil {
			t.Fatalf("Parse(%q) succeeded", expression)
		}
	}
}

func TestNextRejectsExpressionWithoutFutureOccurrences(t *testing.T) {
	if _, err := Next("0 0 31 2 *", time.Now()); err == nil {
		t.Fatal("impossible calendar expression returned a next occurrence")
	}
}
