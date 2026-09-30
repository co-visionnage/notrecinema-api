package stats

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"notrecinema/api/internal/platform/apperror"
)

type Achievement struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Progress    int    `json:"progress"`
	Target      int    `json:"target"`
	Unlocked    bool   `json:"unlocked"`
}

type FamilyAchievements struct {
	TotalWatchedCount  int           `json:"totalWatchedCount"`
	TotalHoursWatched  int           `json:"totalHoursWatched"`
	CurrentStreakWeeks int           `json:"currentStreakWeeks"`
	Achievements       []Achievement `json:"achievements"`
}

func (s *Service) GetFamilyAchievements(ctx context.Context, userID, familyID string) (FamilyAchievements, error) {
	var (
		totalWatchedCount int
		totalHoursWatched int
		weekStarts        []time.Time
	)

	err := s.db.WithUserContext(ctx, userID, func(ctx context.Context, tx pgx.Tx) error {
		var totalHours float64
		if err := tx.QueryRow(ctx, `
			SELECT
				COALESCE(SUM(COALESCE(series.total_episodes, 1) * COALESCE(series.episode_runtime_minutes, 45)) / 60.0, 0),
				COUNT(*)
			FROM public.family_series_status status
			JOIN public.family_series series ON series.id = status.series_id
			WHERE series.family_id = $1 AND status.status = 'watched'
		`, familyID).Scan(&totalHours, &totalWatchedCount); err != nil {
			return err
		}
		totalHoursWatched = int(totalHours + 0.5)

		rows, err := tx.Query(ctx, `
			SELECT DISTINCT DATE_TRUNC('week', COALESCE(status.watched_at, status.updated_at))::date
			FROM public.family_series_status status
			JOIN public.family_series series ON series.id = status.series_id
			WHERE series.family_id = $1 AND status.status = 'watched'
		`, familyID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var weekStart time.Time
			if err := rows.Scan(&weekStart); err != nil {
				return err
			}
			weekStarts = append(weekStarts, weekStart)
		}
		return rows.Err()
	})
	if err != nil {
		return FamilyAchievements{}, apperror.Internal("failed to load achievements", err)
	}

	currentStreakWeeks := computeCurrentStreakWeeks(weekStarts)

	type definition struct {
		id                 string
		title, description string
		progress, target   int
	}

	definitions := []definition{
		{"first-watch", "Первый просмотр", "Отметьте первый сериал или фильм как просмотренный", totalWatchedCount, 1},
		{"watched-10", "10 просмотрено", "Досмотрите 10 сериалов или фильмов", totalWatchedCount, 10},
		{"watched-25", "25 просмотрено", "Досмотрите 25 сериалов или фильмов", totalWatchedCount, 25},
		{"watched-50", "50 просмотрено", "Досмотрите 50 сериалов или фильмов", totalWatchedCount, 50},
		{"hours-10", "10 часов", "Наберите 10 часов совместного просмотра", totalHoursWatched, 10},
		{"hours-50", "50 часов", "Наберите 50 часов совместного просмотра", totalHoursWatched, 50},
		{"hours-100", "100 часов", "Наберите 100 часов совместного просмотра", totalHoursWatched, 100},
		{"streak-4-weeks", "Месяц подряд", "Смотрите что-нибудь каждую неделю 4 недели подряд", currentStreakWeeks, 4},
		{"streak-12-weeks", "Три месяца подряд", "Смотрите что-нибудь каждую неделю 12 недель подряд", currentStreakWeeks, 12},
	}

	achievements := make([]Achievement, len(definitions))
	for i, d := range definitions {
		achievements[i] = Achievement{
			ID:          d.id,
			Title:       d.title,
			Description: d.description,
			Progress:    d.progress,
			Target:      d.target,
			Unlocked:    d.progress >= d.target,
		}
	}

	return FamilyAchievements{
		TotalWatchedCount:  totalWatchedCount,
		TotalHoursWatched:  totalHoursWatched,
		CurrentStreakWeeks: currentStreakWeeks,
		Achievements:       achievements,
	}, nil
}

// computeCurrentStreakWeeks -- прямой перенос одноимённой функции из
// notrecinema-app (src/shared/api/postgres/queries.ts). weekStarts --
// понедельники (UTC) недель, где семья что-то посмотрела; стрик считается
// текущим, только если последняя неделя с просмотром -- эта или прошлая,
// иначе он прерван, даже если когда-то был длинным.
func computeCurrentStreakWeeks(weekStarts []time.Time) int {
	if len(weekStarts) == 0 {
		return 0
	}

	sorted := make([]time.Time, len(weekStarts))
	copy(sorted, weekStarts)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].After(sorted[i]) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	now := time.Now().UTC()
	currentWeekStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	weekday := int(currentWeekStart.Weekday()) // Sunday=0 ... Saturday=6
	diffToMonday := weekday - 1
	if weekday == 0 {
		diffToMonday = 6
	}
	currentWeekStart = currentWeekStart.AddDate(0, 0, -diffToMonday)

	mostRecentGapDays := int(currentWeekStart.Sub(sorted[0]).Hours()/24 + 0.5)
	if mostRecentGapDays > 7 {
		return 0
	}

	streak := 1
	for i := 1; i < len(sorted); i++ {
		gapDays := int(sorted[i-1].Sub(sorted[i]).Hours()/24 + 0.5)
		if gapDays == 7 {
			streak++
		} else {
			break
		}
	}
	return streak
}
