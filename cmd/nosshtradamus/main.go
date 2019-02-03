package main

import (
	"nosshtradamus/internal/sshproxy"

	"flag"
	"net"
)

func main() {
	port := 0
	target := ""

	flag.IntVar(&port, "port", 0, "Proxy listen port")
	flag.StringVar(&target, "target", "", "Target SSH host")
	flag.Parse()

	if port == 0 || target == "" {
		flag.Usage()
		return
	}

	if addr, err := net.ResolveTCPAddr("tcp", target); err != nil {
		panic(err)
	} else {
		sshproxy.DumbTransparentProxy(port, addr)
	}
}
