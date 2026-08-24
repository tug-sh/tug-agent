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

// duplicateConnectionError marks the case where the API rejected this agent
// because another connection is already live on the same server_id. Retrying
// immediately would only re-enter the eviction loop, so the agent cools down.
type duplicateConnectionError struct {
	cause error
}

func (failure duplicateConnectionError) Error() string {
	return failure.cause.Error()
}

func (failure duplicateConnectionError) Unwrap() error {
	return failure.cause
}

func markDuplicateConnection(cause error) error {
	if cause == nil {
		return nil
	}
	return duplicateConnectionError{cause: cause}
}

func isDuplicateConnection(err error) bool {
	var target duplicateConnectionError
	return errors.As(err, &target)
}
