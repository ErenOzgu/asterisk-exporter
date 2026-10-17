// Command amisim runs a simulated Asterisk Manager Interface for demos and
// development. It reports a PBX with twelve PJSIP endpoints, calls that come
// and go and a conference room, all moving with time.
package main

import (
	"flag"
	"log"
	"net"
	"time"

	"github.com/ErenOzgu/asterisk-exporter/internal/amisim"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:5038", "Address to listen on.")
	user := flag.String("user", "exporter", "AMI username to accept.")
	secret := flag.String("secret", "demo-secret", "AMI secret to accept.")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("amisim listening on %s (user %q)", ln.Addr(), *user)
	srv := &amisim.Server{
		Username: *user,
		Secret:   *secret,
		State:    func() amisim.State { return amisim.Demo(time.Now()) },
	}
	log.Fatal(srv.Serve(ln))
}
