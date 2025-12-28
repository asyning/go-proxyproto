package proxyproto

import (
	"errors"
	"golang.org/x/sys/unix"
	"net"
	"time"
)

var (
	notTcpConnErr = errors.New("not a TCP connection")
	connNotFound  = errors.New("connection not found")
)

type TCPFullInfo struct {
	Info       *unix.TCPInfo
	Congestion string
	Duration   time.Duration
}

func (m *Manager) TCPFullInfoByID(id string) (*TCPFullInfo, error) {
	value, ok := m.connections.Load(id)
	if !ok {
		return nil, connNotFound
	}
	c := value
	tcpConn, ok := c.TCPConn()
	if !ok {
		return nil, notTcpConnErr
	}
	info, err := GetTCPFullInfo(tcpConn)
	if err != nil {
		return nil, err
	}
	info.Duration = time.Now().Sub(c.Start)
	return info, nil
}

func GetTCPFullInfo(conn *net.TCPConn) (*TCPFullInfo, error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}

	var info *unix.TCPInfo
	var congestion string
	var sysErr error

	err = rawConn.Control(func(fd uintptr) {
		info, sysErr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if sysErr != nil {
			return
		}

		congestion, sysErr = unix.GetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION)
		if sysErr != nil {
			return
		}
	})
	if err != nil {
		return nil, err
	}
	if sysErr != nil {
		return nil, sysErr
	}

	return &TCPFullInfo{
		Info:       info,
		Congestion: congestion,
	}, nil
}
