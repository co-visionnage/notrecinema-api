package ratelimit

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
)

type fakeRow struct {
	allowed bool
	err     error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*bool)) = r.allowed
	return nil
}

type fakeDB struct {
	row     fakeRow
	gotSQL  string
	gotArgs []any
}

func (f *fakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.gotSQL, f.gotArgs = sql, args
	return f.row
}

func TestCheckPassesArgumentsAndReturnsTheVerdict(t *testing.T) {
	db := &fakeDB{row: fakeRow{allowed: true}}

	allowed, err := Check(context.Background(), db, "auth:ip:203.0.113.7", 30, 900)
	if err != nil || !allowed {
		t.Fatalf("Check() = (%v, %v), want (true, nil)", allowed, err)
	}
	if len(db.gotArgs) != 3 || db.gotArgs[0] != "auth:ip:203.0.113.7" || db.gotArgs[1] != 30 || db.gotArgs[2] != 900 {
		t.Errorf("args = %v", db.gotArgs)
	}

	db.row = fakeRow{allowed: false}
	if allowed, _ := Check(context.Background(), db, "auth:ip:203.0.113.7", 30, 900); allowed {
		t.Error("a rejected check was reported as allowed")
	}
}

func TestCheckReturnsDatabaseErrorsAndFailsClosed(t *testing.T) {
	db := &fakeDB{row: fakeRow{err: errors.New("db down")}}

	allowed, err := Check(context.Background(), db, "auth:ip:x", 1, 1)
	if err == nil {
		t.Fatal("a database error was swallowed")
	}
	if allowed {
		t.Error("on a database error the check must not allow the request")
	}
}

func TestCheckCountsAllowedAndRejectedPerBucketWithoutLeakingTheKey(t *testing.T) {
	db := &fakeDB{row: fakeRow{allowed: true}}
	_, _ = Check(context.Background(), db, "ratelimit-test:ip:198.51.100.1", 5, 60)
	_, _ = Check(context.Background(), db, "ratelimit-test:ip:198.51.100.2", 5, 60)
	db.row = fakeRow{allowed: false}
	_, _ = Check(context.Background(), db, "ratelimit-test:ip:198.51.100.3", 5, 60)

	// Метка -- группа без адреса: иначе на каждый IP был бы отдельный ряд.
	if got := metricValue(t, "ratelimit-test:ip", "allowed"); got != 2 {
		t.Errorf("allowed = %v, want 2", got)
	}
	if got := metricValue(t, "ratelimit-test:ip", "rejected"); got != 1 {
		t.Errorf("rejected = %v, want 1", got)
	}
	if got := metricValue(t, "ratelimit-test:ip:198.51.100.1", "allowed"); got != 0 {
		t.Errorf("the full key leaked into a label (series value %v)", got)
	}
}

// metricValue читает rate_limit_checks_total из реестра по умолчанию.
func metricValue(t *testing.T, bucket, result string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "rate_limit_checks_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["bucket"] == bucket && labels["result"] == result {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}
