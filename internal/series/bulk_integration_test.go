//go:build integration

// Запуск: DATABASE_URL=... go test -tags integration ./...
package series_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/series"
)

func TestCreateBulkInsertsSeriesAndStatus(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := series.NewService(db)

	owner := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)

	rating := 5
	comment := "great show"
	items := []series.BulkItem{
		{Title: "Watched Show", Genres: []string{"drama"}, Year: 2020, Status: "watched", Rating: &rating, Comment: &comment, MediaType: "series"},
		{Title: "To Watch Show", Genres: []string{"comedy"}, Year: 2021, Status: "to-watch", MediaType: "series"},
	}

	result, err := svc.CreateBulk(ctx, owner, familyID, items)
	if err != nil {
		t.Fatalf("CreateBulk() error: %v", err)
	}
	if result.AddedCount != 2 {
		t.Fatalf("AddedCount = %d, want 2", result.AddedCount)
	}

	list, err := svc.ListForFamily(ctx, owner, familyID)
	if err != nil {
		t.Fatalf("ListForFamily() error: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListForFamily() = %d series, want 2: %+v", len(list), list)
	}

	var watchedCount, ratingFound int
	err = db.WithUserContext(ctx, owner, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT status, rating FROM public.family_series_status
			JOIN public.family_series ON family_series.id = family_series_status.series_id
			WHERE family_series.family_id = $1
		`, familyID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var status string
			var rating *int
			if err := rows.Scan(&status, &rating); err != nil {
				return err
			}
			if status == "watched" {
				watchedCount++
				if rating != nil {
					ratingFound = *rating
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("failed to inspect status rows: %v", err)
	}
	if watchedCount != 1 {
		t.Errorf("watched count = %d, want 1", watchedCount)
	}
	if ratingFound != 5 {
		t.Errorf("rating = %d, want 5", ratingFound)
	}
}

func TestCreateBulkRejectsTooManyItems(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := series.NewService(db)

	owner := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)

	items := make([]series.BulkItem, 501)
	for i := range items {
		items[i] = series.BulkItem{Title: "Show", Status: "to-watch", MediaType: "series"}
	}

	if _, err := svc.CreateBulk(ctx, owner, familyID, items); err == nil {
		t.Error("CreateBulk() with 501 items succeeded, want error")
	}
}

func TestCreateBulkEmptyIsNoop(t *testing.T) {
	db := connectOrSkip(t)
	ctx := context.Background()
	svc := series.NewService(db)

	owner := createTestUser(t, ctx, db)
	familyID := createTestFamily(t, ctx, db, owner)

	result, err := svc.CreateBulk(ctx, owner, familyID, nil)
	if err != nil {
		t.Fatalf("CreateBulk() with empty items error: %v", err)
	}
	if result.AddedCount != 0 {
		t.Errorf("AddedCount = %d, want 0", result.AddedCount)
	}
}
