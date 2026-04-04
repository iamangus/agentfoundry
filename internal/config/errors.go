package config

import "errors"

var (
	ErrMissingName         = errors.New("definition: name is required")
	ErrMissingKind         = errors.New("definition: kind is required")
	ErrInvalidKind         = errors.New("definition: kind must be 'agent'")
	ErrMissingSystemPrompt = errors.New("definition: system_prompt is required")
	ErrInvalidScope        = errors.New("definition: scope must be 'global', 'team', or 'user'")
	ErrTeamRequired        = errors.New("definition: team is required when scope is 'team'")
)
