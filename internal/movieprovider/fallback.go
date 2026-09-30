package movieprovider

import (
	"context"
	"errors"
	"fmt"
)

// Fallback пробует провайдеров по порядку и возвращает первый успешный
// результат. Ошибка каждого неудачного провайдера копится, чтобы итоговая
// ошибка (если провалились все) объясняла, что именно пошло не так у
// каждого, а не только у последнего.
type Fallback struct {
	providers []Provider
}

func NewFallback(providers ...Provider) *Fallback {
	return &Fallback{providers: providers}
}

func (f *Fallback) Name() string {
	return "fallback"
}

func (f *Fallback) GetMovie(ctx context.Context, externalID string) (Movie, error) {
	var errs []error

	for _, p := range f.providers {
		movie, err := p.GetMovie(ctx, externalID)
		if err == nil {
			return movie, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
	}

	return Movie{}, fmt.Errorf("movieprovider: все источники недоступны: %w", errors.Join(errs...))
}
