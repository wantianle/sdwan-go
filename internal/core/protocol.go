package core

import protocol "sdwan-go/pkg/protocol"

// Message types
const (
	MsgOPENACK  = protocol.MsgOPENACK
	MsgOPEN     = protocol.MsgOPEN
	MsgTUNSetup = protocol.MsgTUNSetup
	MsgECHOREQ  = protocol.MsgECHOREQ
	MsgECHORESP = protocol.MsgECHORESP
	MsgDATA     = protocol.MsgDATA
)

var (
	PktSign         = protocol.PktSign
	PktSignInPlace  = protocol.PktSignInPlace
	PktVerify       = protocol.PktVerify
	EncryptPassword = protocol.EncryptPassword
	BuildOpenPacket = protocol.BuildOpenPacket
	BuildEchoReq    = protocol.BuildEchoReq
	ParseSessionID  = protocol.ParseSessionID
	ParseOPENACKSeq = protocol.ParseOPENACKSeq
	ParseOPENACK    = protocol.ParseOPENACK
	MsgType         = protocol.MsgType
)

type OPENACKResult = protocol.OPENACKResult

type OpenConfig = protocol.OpenConfig
