package internal

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// fakeRow implements pgx.Row to exercise scanRow/scanApp without a database.
type fakeRow struct {
	values []any
	err    error
}

func (f *fakeRow) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	if len(dest) != len(f.values) {
		return errors.New("fakeRow: dest/values length mismatch")
	}
	for i, v := range f.values {
		switch d := dest[i].(type) {
		case *string:
			if v != nil {
				*d = v.(string)
			}
		case **string:
			if v == nil {
				*d = nil
			} else {
				s := v.(string)
				*d = &s
			}
		case *State:
			if v != nil {
				*d = v.(State)
			}
		case *time.Time:
			if v != nil {
				*d = v.(time.Time)
			}
		case **time.Time:
			if v == nil {
				*d = nil
			} else {
				t := v.(time.Time)
				*d = &t
			}
		case *int:
			if v != nil {
				*d = v.(int)
			}
		default:
			return errors.New("fakeRow: unsupported dest type")
		}
	}
	return nil
}

func TestScanRowFullApp(t *testing.T) {
	now := time.Now()
	vendor := "vendor-x"
	vid := "aplit_1"
	row := &fakeRow{values: []any{
		"a1", "u1", vendor, vid, StateStarted, now, now, nil, nil, nil, 1,
	}}
	app, err := scanRow(row)
	if err != nil {
		t.Fatalf("scanRow: %v", err)
	}
	if app.ID != "a1" || app.UserID != "u1" || app.Vendor != vendor ||
		app.VendorApplicantID != vid || app.State != StateStarted ||
		app.Version != 1 {
		t.Fatalf("unexpected app: %+v", app)
	}
	if !app.CreatedAt.Equal(now) || !app.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps not set: %+v", app)
	}
	if !app.ExpiresAt.IsZero() || !app.ReKYCDueAt.IsZero() || !app.DecidedAt.IsZero() {
		t.Fatalf("nullable timestamps should be zero: %+v", app)
	}
}

func TestScanRowWithNullables(t *testing.T) {
	now := time.Now()
	exp := now.Add(time.Hour)
	due := now.Add(365 * 24 * time.Hour)
	dec := now.Add(time.Minute)
	row := &fakeRow{values: []any{
		"a2", "u2", "v", "vid", StatePass, now, now, exp, due, dec, 3,
	}}
	app, err := scanRow(row)
	if err != nil {
		t.Fatalf("scanRow: %v", err)
	}
	if !app.ExpiresAt.Equal(exp) || !app.ReKYCDueAt.Equal(due) || !app.DecidedAt.Equal(dec) {
		t.Fatalf("nullable timestamps not mapped: %+v", app)
	}
	if app.Version != 3 {
		t.Fatalf("version: %d", app.Version)
	}
}

func TestScanRowScanError(t *testing.T) {
	row := &fakeRow{err: errors.New("scan boom")}
	if _, err := scanRow(row); err == nil {
		t.Fatal("expected scan error")
	}
}

func TestScanAppNoRows(t *testing.T) {
	row := &fakeRow{err: pgx.ErrNoRows}
	_, err := scanApp(row)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestScanAppOK(t *testing.T) {
	row := &fakeRow{values: []any{"a1", "u1", nil, nil, StateStarted, time.Time{}, time.Time{}, nil, nil, nil, 1}}
	app, err := scanApp(row)
	if err != nil {
		t.Fatalf("scanApp: %v", err)
	}
	if app == nil || app.ID != "a1" {
		t.Fatalf("unexpected: %+v", app)
	}
}

func TestErrNoRowsWraps(t *testing.T) {
	if err := errNoRows(pgx.ErrNoRows); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound wrap, got %v", err)
	}
	other := errors.New("other")
	if err := errNoRows(other); err != other {
		t.Fatalf("expected passthrough, got %v", err)
	}
}

func TestNullableString(t *testing.T) {
	if nullableString("") != nil {
		t.Fatal("expected nil for empty string")
	}
	if nullableString("x") != "x" {
		t.Fatal("expected x")
	}
}

func TestNullableTime(t *testing.T) {
	if nullableTime(time.Time{}) != nil {
		t.Fatal("expected nil for zero time")
	}
	now := time.Now()
	if nullableTime(now) != now {
		t.Fatal("expected same time")
	}
}

func TestNewDBApplicationRepoNilSink(t *testing.T) {
	r := NewDBApplicationRepo(nil, nil)
	if r == nil {
		t.Fatal("expected repo")
	}
	if r.sink == nil {
		t.Fatal("expected non-nil noop sink")
	}
}