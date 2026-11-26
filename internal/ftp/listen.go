package ftp

import "net"

// listenLoopback opens a loopback listener for active-mode (PORT) transfers.
func listenLoopback() (net.Listener, int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return l, l.Addr().(*net.TCPAddr).Port, nil
}
