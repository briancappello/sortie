package usage

import (
	"context"
	"errors"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// Puller runs one session's pulls for a source whose Strategy is
// StrategyPull and normalizes what each returns. It is not safe for
// concurrent use: one turn calls it at a time.
type Puller struct {
	source  Source
	bound   time.Duration
	pull    func(context.Context) ([]WindowReading, error)
	stopped bool
}

// NewPuller returns one session's puller for source. Each Pull runs pull
// within bound.
func NewPuller(source Source, bound time.Duration, pull func(context.Context) ([]WindowReading, error)) *Puller {
	return &Puller{source: source, bound: bound, pull: pull}
}

// Pull runs one pull unless ctx is done or an earlier pull returned an error
// wrapping ErrPullUnsupported. It returns the readings normalized by
// Windows, observed when pull returned, or nil when there are none. The
// error is pull's.
func (p *Puller) Pull(ctx context.Context) ([]domain.UsageWindow, error) {
	if p.stopped || ctx.Err() != nil {
		return nil, nil
	}
	pullCtx, cancel := context.WithTimeout(ctx, p.bound)
	defer cancel()
	readings, err := p.pull(pullCtx)
	at := time.Now().UTC()
	if errors.Is(err, ErrPullUnsupported) {
		p.stopped = true
	}
	if err != nil {
		return nil, err
	}
	return Windows(p.source, readings, at), nil
}
