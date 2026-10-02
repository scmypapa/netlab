package environment

import "fmt"

type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

func Invalid(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}
