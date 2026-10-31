package native

import "errors"

// ErrDisplayRecoveryFailed marks a virtual-display resize whose rollback also
// failed. The active session must be closed so capture and input do not keep
// targeting an unknown display mode.
var ErrDisplayRecoveryFailed = errors.New("虚拟屏幕模式恢复失败")
