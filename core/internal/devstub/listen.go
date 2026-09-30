package devstub

import "net"

// listenLocal binds a loopback listener on an OS-assigned port.
//
// Binding first and reading the address afterwards avoids the race in picking
// a port number and then listening on it, which is how two demo instances end
// up fighting over 8080.
func listenLocal() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}
