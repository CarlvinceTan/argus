// Command argus-web serves the audit console.
//
// The console is a normal web app: it can run on the machine that performs the
// audits and be opened from anywhere that can reach it, which is what makes the
// TUI, GUI and remote-host workflows the same tool. It serves the Svelte single
// page app, the JSON API the app uses, the evidence files each run produced, and
// an endpoint to start a new audit.
//
//	argus-web --addr 127.0.0.1:8787
//	argus-web --addr 0.0.0.0:8787 --token "$(openssl rand -hex 16)"
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/carlvincetan/argus/internal/webui"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (use 0.0.0.0:8787 to serve the network)")
	token := flag.String("token", os.Getenv("ARGUS_WEB_TOKEN"), "bearer token required for actions (mandatory off-loopback)")
	roots := flag.String("roots", "", "comma separated run directories to offer (default: var/runs and benchmark/results)")
	argusBin := flag.String("argus-bin", "", "path to the argus executable used to start audits")
	flag.Parse()

	service := webui.NewService()
	if *argusBin != "" {
		service.SetArgusBinary(*argusBin)
	}
	options := webui.Options{Addr: *addr, Token: *token}
	if strings.TrimSpace(*roots) != "" {
		for _, root := range strings.Split(*roots, ",") {
			if trimmed := strings.TrimSpace(root); trimmed != "" {
				options.Roots = append(options.Roots, trimmed)
			}
		}
	}
	server, err := webui.NewServer(service, options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "argus-web: %v\n", err)
		os.Exit(1)
	}
	log.Printf("argus-web: serving %s", server.URL())
	if *token == "" {
		log.Printf("argus-web: no token set; only local actions allowed")
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("argus-web: stopping")
		os.Exit(0)
	}()
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("argus-web: %v", err)
	}
}
