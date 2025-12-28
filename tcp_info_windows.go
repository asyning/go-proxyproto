package proxyproto

import "errors"

var (
	notSupportOnWindows = errors.New("notSupportOnWindows")
)

type TCPFullInfo struct {
}

func (m *Manager) TCPFullInfoByID(id string) (*TCPFullInfo, error) {
	return nil, notSupportOnWindows
}
