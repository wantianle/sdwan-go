package controlapi

type StatusResult struct {
	State     string `json:"state"`
	Server    string `json:"server"`
	Port      int    `json:"port"`
	SessionID uint16 `json:"session_id"`
	TUN       string `json:"tun"`
	LocalIP   string `json:"local_ip"`
	GatewayIP string `json:"gateway_ip"`
	Route     string `json:"route"`
	MTU       int    `json:"mtu"`
	// Adding Phase 3-friendly fields now (zero values until populated):
	RouteConflicts []string `json:"route_conflicts,omitempty"`
}

type SwitchResponse struct {
	Status *StatusResult `json:"status"`
	Tunnel *TunnelInfo   `json:"tunnel,omitempty"`
}

type TunnelInfo struct {
	LocalIP   string `json:"local_ip"`
	GatewayIP string `json:"gateway_ip"`
}

type PauseRequest struct {
	Pause bool `json:"pause"`
}

type PauseResponse struct {
	Status *StatusResult `json:"status"`
	Paused bool          `json:"paused"`
}
