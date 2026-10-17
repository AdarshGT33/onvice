package queue

import (
	"context"
	"fmt"

	"github.com/AdarshGT33/onvice/internal/store"
)

func BuildHandler(ctx context.Context, job store.Job) error {
	// TODO:
	// 1. Load deployment details.
	// 2. Clone or fetch the repository.
	// 3. Build the customer's application in an isolated environment.
	// 4. Save build output and artifact details.

	if err := ctx.Err(); err != nil {
		return err
	}

	fmt.Printf("Building deployment %s\n", job.DeploymentID)
	return nil
}
