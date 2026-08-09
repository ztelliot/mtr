package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/ztelliot/mtr/internal/config"
)

type postgresTestRow []any

func (r postgresTestRow) Scan(dest ...any) error {
	if len(dest) != len(r) {
		return fmt.Errorf("scan destination count = %d, want %d", len(dest), len(r))
	}
	for i := range dest {
		target := reflect.ValueOf(dest[i])
		if target.Kind() != reflect.Pointer || target.IsNil() {
			return fmt.Errorf("scan destination %d is not a non-nil pointer", i)
		}
		target.Elem().Set(reflect.ValueOf(r[i]))
	}
	return nil
}

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

func TestPostgresOutboundAgentHuaweiCloudAKSKJSONRoundTrip(t *testing.T) {
	auth := config.OutboundAgentAuth{
		Type: config.OutboundAuthHuaweiCloudAKSK, AccessKey: "test-access-key",
		ProjectID: "test-project-id", Secret: "test-secret-key",
	}
	authRaw, err := marshalJSONBytes(auth)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	row := postgresTestRow{
		"functiongraph-edge", config.OutboundTransportEvent, true,
		"https://functiongraph.example/invoke", []byte(`{}`), authRaw,
		config.EventResponseResultField, []byte(`[]`), []byte(`{}`), now, now,
	}
	got, err := scanOutboundAgent(row)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Auth, auth) {
		t.Fatalf("auth round trip = %#v, want %#v", got.Auth, auth)
	}
}
