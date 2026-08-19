package agent

import "errors"

// nonRetriableError marks failures where reconnecting cannot help, such as an
// invalid or revoked agent token.
type nonRetriableError struct {
	cause error
}

func (failure nonRetriableError) Error() string {
	return failure.cause.Error()
}

func (failure nonRetriableError) Unwrap() error {
	return failure.cause
}

func markNonRetriable(cause error) error {
	if cause == nil {
		return nil
	}
	return nonRetriableError{cause: cause}
}

func isNonRetriable(err error) bool {
	var target nonRetriableError
	return errors.As(err, &target)
}

// pendingAuthError marks failures where the host is waiting for the dashboard
// pairing to complete, so the agent retries quickly instead of backing off.
type pendingAuthError struct {
	cause error
}

func (failure pendingAuthError) Error() string {
	return failure.cause.Error()
}

func (failure pendingAuthError) Unwrap() error {
	return failure.cause
}

func markPendingAuth(cause error) error {
	if cause == nil {
		return nil
	}
	return pendingAuthError{cause: cause}
}

func isPendingAuthError(err error) bool {
	var target pendingAuthError
	return errors.As(err, &target)
}
