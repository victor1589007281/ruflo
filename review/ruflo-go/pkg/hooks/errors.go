package hooks

import "errors"

var (
	ErrInvalidRegistration = errors.New("hooks: invalid registration")
	ErrDuplicateName       = errors.New("hooks: duplicate hook name")
	ErrHookNotFound        = errors.New("hooks: hook not found")
	ErrPatternNotFound     = errors.New("hooks: pattern not found")
)
