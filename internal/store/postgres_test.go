package store

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresMaintenanceDatabases(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   []string
	}{
		{name: "default", target: "mtr", want: []string{"postgres", "template1"}},
		{name: "postgres", target: "postgres", want: []string{"template1"}},
		{name: "template1", target: "template1", want: []string{"postgres"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := postgresMaintenanceDatabases(tt.target)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("got[%d] = %q, want %q (%v)", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

func TestIsPostgresCode(t *testing.T) {
	err := &pgconn.PgError{Code: "3D000"}
	if !isPostgresCode(err, "3D000") {
		t.Fatal("expected matching postgres code")
	}
	if isPostgresCode(err, "42P04") {
		t.Fatal("did not expect mismatched postgres code")
	}
	if isPostgresCode(errors.New("boom"), "3D000") {
		t.Fatal("plain error should not match postgres code")
	}
}
