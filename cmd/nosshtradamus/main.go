package main

import (
	"nosshtradamus/internal/predictive"
	"nosshtradamus/internal/sshproxy"

	"flag"
	"fmt"
	"net"
)

func main() {
	port := 0
	target := ""
	printPredictiveVersion := false

	flag.IntVar(&port, "port", 0, "Proxy listen port")
	flag.StringVar(&target, "target", "", "Target SSH host")
	flag.BoolVar(&printPredictiveVersion, "version", false, "Display predictive backend version")
	flag.Parse()

	if printPredictiveVersion {
		fmt.Printf("Predictive Backend Version: %v\n", predictive.GetVersion())
	}

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
