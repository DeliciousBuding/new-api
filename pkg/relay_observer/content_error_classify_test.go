package relayobserver

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// TestIsDeterministicContentError locks the poison-classification
// contract: only PostgreSQL data exceptions (22xxx), integrity constraint
// violations (23xxx), and datatype mismatches (42804) are permanent poison
// that must be dropped; every transient failure (deadlock, serialization,
// query cancellation) and unknown error must be retained.
func TestIsDeterministicContentError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not poison", nil, false},
		{"generic error is not poison", errors.New("boom"), false},
		{"deadlock is transient", &pgconn.PgError{Code: "40P01"}, false},
		{"serialization failure is transient", &pgconn.PgError{Code: "40001"}, false},
		{"query cancellation is transient", &pgconn.PgError{Code: "57014"}, false},
		{"connection exception is transient", &pgconn.PgError{Code: "08006"}, false},
		{"unique violation is poison", &pgconn.PgError{Code: "23505"}, true},
		{"foreign key violation is poison", &pgconn.PgError{Code: "23503"}, true},
		{"check violation is poison", &pgconn.PgError{Code: "23514"}, true},
		{"not null violation is poison", &pgconn.PgError{Code: "23502"}, true},
		{"invalid text representation is poison", &pgconn.PgError{Code: "22P02"}, true},
		{"datatype mismatch is poison", &pgconn.PgError{Code: "42804"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isDeterministicContentError(tc.err))
		})
	}
}
