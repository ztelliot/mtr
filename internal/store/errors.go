package store

import "errors"

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")
var ErrInvalidRegisterToken = errors.New("invalid agent registration token")
var ErrRegisterTokenExhausted = errors.New("agent registration token has no remaining uses")
var ErrAgentIDNotAllowed = errors.New("agent id is not allowed by registration token")
