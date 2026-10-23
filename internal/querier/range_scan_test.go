package querier

import (
	"context"
	"reflect"
	"testing"
)

func TestScanRangeMatchesDirectQueries(t *testing.T) {
	q := setupMessageFixture(t)
	ctx := context.Background()
	dates := []string{"2026-07-09", "2026-07-10"}

	specs := []AggSpec{
		{Name: "client", Kind: AggDimension, Dimensions: []string{"client"}},
		{Name: "model", Kind: AggDimension, Dimensions: []string{"model"}},
		{Kind: AggDayHour},
		{Kind: AggSession},
	}
	snap, err := q.ScanRange(ctx, dates, specs)
	if err != nil {
		t.Fatalf("ScanRange failed: %v", err)
	}

	stats, err := q.StatsBetween(ctx, dates[0], dates[len(dates)-1])
	if err != nil {
		t.Fatalf("StatsBetween failed: %v", err)
	}
	if got, want := snap.Totals().ActiveDays, stats.ActiveDays; got != want {
		t.Errorf("active days = %d, want %d", got, want)
	}
	if got, want := snap.Totals().GroupAggregate, stats.Total; got != want {
		t.Errorf("totals = %+v, want %+v", got, want)
	}

	for _, name := range []string{"client", "model"} {
		rows, totals, err := q.AggregateDimensionView(ctx, dates, DimensionView{
			Dimensions: []string{name}, TitleEn: name, TitleZh: name,
		})
		if err != nil {
			t.Fatalf("AggregateDimensionView(%s) failed: %v", name, err)
		}
		got, ok := snap.Dimension(name)
		if !ok {
			t.Fatalf("dimension %q not found", name)
		}
		if !reflect.DeepEqual(got.Rows, rows) {
			t.Errorf("dimension %q rows mismatch\ngot  %+v\nwant %+v", name, got.Rows, rows)
		}
		if got.Totals != totals {
			t.Errorf("dimension %q totals = %+v, want %+v", name, got.Totals, totals)
		}
		if got.Totals != snap.Totals().GroupAggregate {
			t.Errorf("dimension %q totals should reuse range totals", name)
		}
	}

	wantMatrix, err := q.DayHourMatrix(ctx, dates)
	if err != nil {
		t.Fatalf("DayHourMatrix failed: %v", err)
	}
	gotMatrix, ok := snap.DayHour()
	if !ok {
		t.Fatal("day-hour matrix not found")
	}
	if !reflect.DeepEqual(gotMatrix, wantMatrix) {
		t.Errorf("day-hour matrix mismatch\ngot  %+v\nwant %+v", gotMatrix, wantMatrix)
	}

	wantSessions, err := q.SessionRows(ctx, dates)
	if err != nil {
		t.Fatalf("SessionRows failed: %v", err)
	}
	gotSessions, ok := snap.Sessions()
	if !ok {
		t.Fatal("sessions not found")
	}
	if !reflect.DeepEqual(gotSessions, wantSessions) {
		t.Errorf("sessions mismatch\ngot  %+v\nwant %+v", gotSessions, wantSessions)
	}
}

func TestScanRangeValidation(t *testing.T) {
	q := setupMessageFixture(t)
	ctx := context.Background()
	dates := []string{"2026-07-09"}

	cases := []struct {
		name  string
		specs []AggSpec
	}{
		{"empty dimension name", []AggSpec{{Kind: AggDimension, Dimensions: []string{"client"}}}},
		{"no dimensions", []AggSpec{{Name: "x", Kind: AggDimension}}},
		{"unknown dimension", []AggSpec{{Name: "x", Kind: AggDimension, Dimensions: []string{"nope"}}}},
		{"duplicate dimension", []AggSpec{{Name: "x", Kind: AggDimension, Dimensions: []string{"client", "client"}}}},
		{"duplicate result name", []AggSpec{
			{Name: "x", Kind: AggDimension, Dimensions: []string{"client"}},
			{Name: "x", Kind: AggDimension, Dimensions: []string{"model"}},
		}},
		{"duplicate day-hour", []AggSpec{{Kind: AggDayHour}, {Kind: AggDayHour}}},
		{"duplicate session", []AggSpec{{Kind: AggSession}, {Kind: AggSession}}},
		{"unknown kind", []AggSpec{{Name: "x", Kind: AggKind(99)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := q.ScanRange(ctx, dates, tc.specs); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestScanRangeEmptyDates(t *testing.T) {
	q := setupMessageFixture(t)
	ctx := context.Background()

	snap, err := q.ScanRange(ctx, nil, []AggSpec{
		{Name: "client", Kind: AggDimension, Dimensions: []string{"client"}},
		{Kind: AggSession},
	})
	if err != nil {
		t.Fatalf("ScanRange failed: %v", err)
	}
	if got := snap.Totals(); got.ActiveDays != 0 || got.Requests != 0 {
		t.Errorf("empty range totals = %+v, want zero", got)
	}
	dim, ok := snap.Dimension("client")
	if !ok {
		t.Fatal("client dimension not found")
	}
	if dim.Rows == nil || len(dim.Rows) != 0 {
		t.Errorf("client rows = %#v, want empty non-nil slice", dim.Rows)
	}
	sessions, ok := snap.Sessions()
	if !ok {
		t.Fatal("sessions not found")
	}
	if sessions == nil || len(sessions) != 0 {
		t.Errorf("sessions = %#v, want empty non-nil slice", sessions)
	}
	if _, ok := snap.DayHour(); ok {
		t.Error("day-hour should not be declared")
	}
}

func TestScanRangeOptionalKinds(t *testing.T) {
	q := setupMessageFixture(t)
	ctx := context.Background()

	snap, err := q.ScanRange(ctx, []string{"2026-07-09"}, []AggSpec{
		{Name: "client", Kind: AggDimension, Dimensions: []string{"client"}},
	})
	if err != nil {
		t.Fatalf("ScanRange failed: %v", err)
	}
	if _, ok := snap.DayHour(); ok {
		t.Error("day-hour not declared but found")
	}
	if _, ok := snap.Sessions(); ok {
		t.Error("sessions not declared but found")
	}
	if _, ok := snap.Dimension("model"); ok {
		t.Error("model not declared but found")
	}
}
