// Command stt-router is a self-hosted speech-to-text account router.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Jamir-boop/stt-router/internal/config"
	"github.com/Jamir-boop/stt-router/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const shutdownTimeout = 60 * time.Second

func main() {
	configPath := flag.String("config", config.DefaultPath(), "path to the JSON config file")
	listen := flag.String("listen", "", "listen address, overrides the config (for example 127.0.0.1:8330)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		fmt.Fprint(flag.CommandLine.Output(), usage, "Options:\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("stt-router", version)
		return
	}
	var err error
	if cmd := flag.Arg(0); cmd == "" || cmd == "serve" {
		err = run(*configPath, *listen)
	} else {
		err = cli(cmd, *configPath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stt-router:", err)
		os.Exit(1)
	}
}

func run(configPath, listen string) error {
	cfg, created, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if created {
		log.Printf("created default config at %s", configPath)
	}
	if listen == "" {
		listen = cfg.Listen
	}
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return fmt.Errorf("listen %q: must be host:port", listen)
	}
	key, err := server.EnsureStartup(configPath, cfg, listen)
	if err != nil {
		return err
	}
	if key != "" {
		// Printed once on stdout; it is also saved in the config file.
		fmt.Printf("Generated management key (saved in %s):\n%s\n", configPath, key)
	}

	srv, err := server.New(cfg, server.Options{ConfigPath: configPath, Listen: listen, Version: version})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		srv.Close()
		return err
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("stt-router %s listening on http://%s", version, ln.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()

	select {
	case err := <-errc:
		srv.Close()
		return err
	case <-ctx.Done():
	}
	// A realtime socket lives until the client closes it, so Shutdown would
	// wait for every one of them; close them first.
	srv.CloseRealtime()
	log.Printf("shutting down; waiting up to %s for requests in flight", shutdownTimeout)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err = hs.Shutdown(sctx)
	if cerr := srv.Close(); cerr != nil {
		log.Printf("flush state: %v", cerr)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
