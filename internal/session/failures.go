package session

import (
	"errors"

	"remotemac/internal/native"
)

var ErrSessionRecoveryFailed = errors.New("桌面会话恢复失败")

// ErrSessionCleanupPending means an old native session is still being
// released. The signaling connection should close so the browser can retry
// with its normal backoff instead of treating this transient race as fatal.
var ErrSessionCleanupPending = errors.New("上一桌面会话仍在回收")

// IsFatalError reports whether the signaling connection must be closed because
// the active display or capture state can no longer be trusted.
func IsFatalError(err error) bool {
	return errors.Is(err, ErrSessionRecoveryFailed) || errors.Is(err, native.ErrDisplayRecoveryFailed)
}

func IsRetryableError(err error) bool {
	return errors.Is(err, ErrSessionCleanupPending)
}
