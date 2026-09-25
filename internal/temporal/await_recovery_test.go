package temporal

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAwaitWorkflowRetriesTransportFailure(t *testing.T) {
	calls := 0
	result, err := awaitWorkflow(context.Background(), false, 0, func(out *RunAgentResult) error {
		calls++
		switch calls {
		case 1:
			return serviceerror.NewUnavailable("temporary outage")
		case 2:
			return status.Error(codes.DeadlineExceeded, "temporary timeout")
		case 3:
			return serviceerror.NewNotFound("start not visible yet")
		default:
			out.Response = "finished"
			return nil
		}
	})
	if err != nil || result == nil || result.Response != "finished" || calls != 4 {
		t.Fatalf("workflow was prematurely failed: result=%+v err=%v calls=%d", result, err, calls)
	}
}

func TestAwaitWorkflowDoesNotRetryExecutionFailure(t *testing.T) {
	failure := errors.New("workflow failed")
	calls := 0
	_, err := awaitWorkflow(context.Background(), true, 0, func(*RunAgentResult) error {
		calls++
		return failure
	})
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("workflow failure was retried: err=%v calls=%d", err, calls)
	}
}

func TestAwaitWorkflowStopsRetryingAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := awaitWorkflow(ctx, true, time.Hour, func(*RunAgentResult) error {
		calls++
		cancel()
		return serviceerror.NewUnavailable("offline")
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("canceled watcher continued: err=%v calls=%d", err, calls)
	}
}

func TestAwaitWorkflowBoundsMissingStart(t *testing.T) {
	calls := 0
	_, err := awaitWorkflow(context.Background(), false, 0, func(*RunAgentResult) error {
		calls++
		return serviceerror.NewNotFound("missing")
	})
	var notFound *serviceerror.NotFound
	if !errors.As(err, &notFound) || calls != 61 {
		t.Fatalf("missing start was not resolved: err=%v calls=%d", err, calls)
	}
}
