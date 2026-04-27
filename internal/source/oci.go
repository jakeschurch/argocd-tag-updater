package source

import (
	"context"
	"fmt"
)

// OCI is a stub. Implement using the Docker Registry V2 API
// (distribution/v3 or google/go-containerregistry) when needed.
type OCI struct {
	Repo string
}

func (o *OCI) Tags(_ context.Context) ([]string, error) {
	return nil, fmt.Errorf("OCI source not yet implemented for repo %s", o.Repo)
}
