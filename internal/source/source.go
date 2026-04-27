package source

import "context"

type Source interface {
	Tags(ctx context.Context) ([]string, error)
}
