package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "1.0.0"

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON configuration file")
	listenOverride := flag.String("listen", "", "override listen address, e.g. 127.0.0.1:9090")
	adminOverride := flag.String("admin-listen", "", "override admin address, or 'off' to disable it")
	checkConfig := flag.Bool("check", false, "validate config, print the effective pool, and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("proxy-rotator %s\n", version)
		return
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "proxy-rotator: %v\n", err)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "hint: copy config.example.json to config.json and edit the upstream pool")
		}
		os.Exit(1)
	}
	if *listenOverride != "" {
		if err := validateListenAddr(*listenOverride); err != nil {
			fmt.Fprintf(os.Stderr, "proxy-rotator: -listen: %v\n", err)
			os.Exit(1)
		}
		cfg.Listen = *listenOverride
	}
	if *adminOverride != "" {
		if *adminOverride == "off" || *adminOverride == "none" {
			cfg.AdminListen = ""
		} else {
			if err := validateListenAddr(*adminOverride); err != nil {
				fmt.Fprintf(os.Stderr, "proxy-rotator: -admin-listen: %v\n", err)
				os.Exit(1)
			}
			cfg.AdminListen = *adminOverride
		}
	}

	log := NewLogger(os.Stdout, cfg.LogLevel)
	if *checkConfig {
		log.infof("validating configuration: %s", *configPath)
	} else {
		log.infof("proxy-rotator %s starting (config: %s)", version, *configPath)
	}

	for _, u := range cfg.Upstreams {
		auth := "pass-through"
		if u.APIKey != "" {
			auth = "configured"
		}
		if *checkConfig {
			log.infof("upstream [%s] priority=%d url=%s auth=%s", u.Name, u.Priority, sanitizeLog(u.displayURL(), 200), auth)
		} else {
			log.debugf("", "pool [%s prio=%d] %s auth=%s", u.Name, u.Priority, sanitizeLog(u.displayURL(), 200), auth)
		}

		if u.APIKey == "" && !isLoopbackHost(u.Base.Hostname()) {
			log.warnf("", "upstream %q has no api_key; the client's Authorization header will be forwarded to %s",
				u.Name, u.Base.Host)
		}
	}

	if cfg.ClientAPIKey != "" {
		log.debugf("", "client_api_key is enabled on the proxy port")
	}
	if !isLoopbackListen(cfg.Listen) {
		if cfg.ClientAPIKey == "" {
			log.warnf("", "listening on %s (not loopback) without client_api_key: anyone who can reach it can spend your upstream credits", cfg.Listen)
		} else {
			log.warnf("", "listening on %s (not loopback); client_api_key is required, but consider a firewall too", cfg.Listen)
		}
	}
	if cfg.AdminListen != "" && !isLoopbackListen(cfg.AdminListen) {
		log.warnf("", "admin endpoint on %s is not loopback; it exposes pool status without authentication", cfg.AdminListen)
	}

	effectiveAttempts := cfg.MaxAttempts
	if effectiveAttempts <= 0 || effectiveAttempts > len(cfg.Upstreams) {
		effectiveAttempts = len(cfg.Upstreams)
	}
	log.infof("pool: %d upstream(s), up to %d attempt(s) per request", len(cfg.Upstreams), effectiveAttempts)
	if *checkConfig {
		log.infof("configuration OK; not starting the proxy")
		return
	}

	proxyLn, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.errorf("", "cannot listen on %s: %v", cfg.Listen, err)
		os.Exit(1)
	}
	defer proxyLn.Close()

	var adminLn net.Listener
	if cfg.AdminListen != "" {
		adminLn, err = net.Listen("tcp", cfg.AdminListen)
		if err != nil {
			log.errorf("", "cannot listen on admin address %s: %v", cfg.AdminListen, err)
			os.Exit(1)
		}
		defer adminLn.Close()
	}

	p := NewProxy(cfg, log)
	proxySrv := &http.Server{
		Handler:           p.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// ReadTimeout/WriteTimeout intentionally unset: prompts and SSE
		// streams can legitimately run for many minutes.
	}
	adminSrv := &http.Server{
		Handler:           p.AdminHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	errCh := make(chan error, 2)
	go func() {
		if err := proxySrv.Serve(proxyLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("proxy server: %w", err)
		}
	}()
	if adminLn != nil {
		go func() {
			if err := adminSrv.Serve(adminLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("admin server: %w", err)
			}
		}()
	}

	log.infof("listening on http://%s", proxyLn.Addr())
	if adminLn != nil {
		log.infof("admin on http://%s/status (POST /reset resets cooldowns)", adminLn.Addr())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	exitCode := 0
	select {
	case <-ctx.Done():
		log.infof("shutdown signal received")
	case err := <-errCh:
		log.errorf("", "%v", err)
		exitCode = 1
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = proxySrv.Shutdown(shutdownCtx)
	if adminLn != nil {
		_ = adminSrv.Shutdown(shutdownCtx)
	}
	log.infof("stopped")
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}
