package digest

import (
	"testing"
	"time"
)

func TestParseWeekday(t *testing.T) {
	cases := map[string]time.Weekday{
		"monday": time.Monday, "Monday": time.Monday, " mon ": time.Monday,
		"sunday": time.Sunday, "Sat": time.Saturday, "FRIDAY": time.Friday,
	}
	for in, want := range cases {
		got, err := ParseWeekday(in)
		if err != nil || got != want {
			t.Errorf("ParseWeekday(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "понедельник", "someday", "mo"} {
		if _, err := ParseWeekday(bad); err == nil {
			t.Errorf("ParseWeekday(%q) accepted", bad)
		}
	}
}

func TestWeekKeyUsesTheISOWeek(t *testing.T) {
	cases := map[time.Time]string{
		time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC):   "2026-W41",
		time.Date(2026, 10, 11, 23, 0, 0, 0, time.UTC): "2026-W41",
		time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC):  "2026-W42",
		// ISO-неделя может принадлежать соседнему году.
		time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC): "2026-W01",
		time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC): "2026-W53",
	}
	for at, want := range cases {
		if got := weekKey(at); got != want {
			t.Errorf("weekKey(%v) = %q, want %q", at, got, want)
		}
	}
}

func TestDueOnlyOnTheConfiguredDayFromTheConfiguredHour(t *testing.T) {
	runner := NewRunner(nil, nil, time.Minute, time.Monday, 6)

	cases := []struct {
		at   time.Time
		want bool
	}{
		{time.Date(2026, 10, 5, 5, 59, 0, 0, time.UTC), false}, // понедельник, рано
		{time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC), true},
		{time.Date(2026, 10, 5, 23, 30, 0, 0, time.UTC), true}, // API поднялся поздно, но в тот же день
		{time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC), false},  // вторник
		{time.Date(2026, 10, 4, 7, 0, 0, 0, time.UTC), false},  // воскресенье
	}
	for _, c := range cases {
		if got := runner.due(c.at); got != c.want {
			t.Errorf("due(%v) = %v, want %v", c.at, got, c.want)
		}
	}
}

func TestDueConvertsToUTC(t *testing.T) {
	runner := NewRunner(nil, nil, time.Minute, time.Monday, 6)
	moscow := time.FixedZone("MSK", 3*3600)

	// 08:30 в Москве -- ещё 05:30 UTC, рано.
	if runner.due(time.Date(2026, 10, 5, 8, 30, 0, 0, moscow)) {
		t.Error("08:30 MSK is 05:30 UTC and must not be due")
	}
	if !runner.due(time.Date(2026, 10, 5, 9, 0, 0, 0, moscow)) {
		t.Error("09:00 MSK is 06:00 UTC and must be due")
	}
}
