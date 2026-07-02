package core

import controlapi "sdwan-go/pkg/controlapi"

var (
	LoadControlToken = controlapi.LoadControlToken
	ControlStatus    = controlapi.ControlStatus
	ControlSwitch    = controlapi.ControlSwitch
	ControlShutdown  = controlapi.ControlShutdown
)

type (
	StatusResult   = controlapi.StatusResult
	SwitchResponse = controlapi.SwitchResponse
	TunnelInfo     = controlapi.TunnelInfo
	PauseRequest   = controlapi.PauseRequest
	PauseResponse  = controlapi.PauseResponse
)
