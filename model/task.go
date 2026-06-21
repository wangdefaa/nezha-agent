package model

// task type 数值是与后端的协议约定。删除中间功能（命令执行/终端/NAT/文件管理/
// 配置下发/服务器转移/MCP）后保留原数值、不复用空缺号段，避免破坏与后端的兼容。
const (
	TaskTypeHTTPGet   = 1
	TaskTypeICMPPing  = 2
	TaskTypeTCPPing   = 3
	TaskTypeUpgrade   = 6
	TaskTypeKeepalive = 7
)
