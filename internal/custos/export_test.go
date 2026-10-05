package custos

// SetKillPointHook configures the crash-injection seam for V25 tests.
// Available only in test binaries.
func SetKillPointHook(hook func(string)) {
	killPointHook = hook
}
