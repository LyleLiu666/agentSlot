package workflow

import (
	"context"
	"errors"
)

// WaitTerminal follows Scheduler updates until the job reaches a terminal
// state. Scheduler.Wait intentionally returns any version newer than after,
// including the transition from queued to running.
func WaitTerminal(ctx context.Context, scheduler Scheduler, jobID string, after uint64) (Job, error) {
	if scheduler == nil {
		return Job{}, errors.New("workflow: scheduler is required")
	}
	for {
		job, err := scheduler.Wait(ctx, jobID, after)
		if err != nil {
			return Job{}, err
		}
		if job.Status.Terminal() {
			return job, nil
		}
		if job.Version <= after {
			return Job{}, errors.New("workflow: scheduler returned a non-advancing job version")
		}
		after = job.Version
	}
}
