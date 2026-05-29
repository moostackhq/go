package cli

import "fmt"

// UsageError formats a message and wraps [ErrUsage]. Use from
// handler code when the caller's invocation is the cause. The
// default error renderer appends the relevant command's help to the
// output.
func UsageError(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrUsage)
}

// ValidationError formats a message and wraps [ErrValidation].
func ValidationError(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrValidation)
}
