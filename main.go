// Command browser-print-agentd is the macOS local print agent: a loopback daemon
// that speaks the Zebra Browser Print wire contract on top of CUPS so the SPA's
// direct-print transport works on a station with no Zebra software installed.
//
// It is a faithful port of tools/browser-print-shim/shim.py with three
// deliberate upgrades the shim does not need: printers are DISCOVERED from CUPS
// (`lpstat -v`) instead of hand-listed, health is checked at job initiation with
// USB-to-network failover instead of a hard failure, and every request's Origin
// is logged (with an optional allowlist enforced on /write).
//
// Usage:
//
//	browser-print-agentd
//	browser-print-agentd --port 9100 --https-port 9101 --origin-allow https://lab.example
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// shutdownGrace bounds how long in-flight requests get to finish on shutdown.
const shutdownGrace = 5 * time.Second

func main() {
	logger := newAgentLogger(os.Stdout)
	if path := os.Getenv(logPathEnvVar); path != "" {
		var err error
		logger, err = newRotatingAgentLogger(path)
		if err != nil {
			reportFallback(fmt.Sprintf("cannot initialize agent log: %v", err))
			os.Exit(1)
		}
		defer logger.close()
	}

	config, err := parseConfig(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		// `--help` prints usage through the flag set itself, so it exits clean
		// rather than looking like a misconfigured LaunchAgent.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		logger.write(fmt.Sprintf("error: %v", err))
		os.Exit(2)
	}
	if err := run(config, logger); err != nil {
		logger.write(fmt.Sprintf("error: %v", err))
		os.Exit(1)
	}
}

// run starts the listeners and blocks until the process is asked to stop.
func run(config config, logger *agentLogger) error {
	if err := verifyCUPSBinaries(); err != nil {
		return err
	}

	handler := newAgent(osRunner{}, logger, config.OriginAllow)
	if config.UpdateStatusPath != "" && config.UpdaterLabel != "" {
		handler.updates = &updateReader{
			path:  config.UpdateStatusPath,
			label: config.UpdaterLabel,
		}
	}

	if len(config.OriginAllow) == 0 {
		logger.write("origin posture: log-and-allow (no --origin-allow configured)")
	} else {
		logger.write(fmt.Sprintf("origin posture: /write restricted to %v",
			config.OriginAllow))
	}

	servers := make([]*http.Server, 0, 2)
	errs := make(chan error, 2)

	plain := &http.Server{
		Addr:              net.JoinHostPort(config.Bind, strconv.Itoa(config.Port)),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	servers = append(servers, plain)
	go func() {
		logger.write(fmt.Sprintf("listening on http://%s", plain.Addr))
		errs <- plain.ListenAndServe()
	}()

	// The HTTPS listener is gated on the cert pair exactly like the shim: no
	// cert means no :9101, which the SPA detects and turns into the Safari
	// setup walkthrough rather than a silent failure.
	certFile, keyFile := config.certPaths()
	if fileExists(certFile) && fileExists(keyFile) {
		secure := &http.Server{
			Addr:              net.JoinHostPort(config.Bind, strconv.Itoa(config.HTTPSPort)),
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		}
		servers = append(servers, secure)
		go func() {
			logger.write(fmt.Sprintf("listening on https://%s", secure.Addr))
			errs <- secure.ListenAndServeTLS(certFile, keyFile)
		}()
	} else {
		logger.write(fmt.Sprintf(
			"no cert pair in %s - HTTPS listener skipped; Safari cannot reach the "+
				"agent without it", config.CertDir))
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	var runErr error
	select {
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	case <-stop:
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	for _, server := range servers {
		server.Shutdown(ctx)
	}
	return runErr
}

// reportFallback is used only when the daemon-owned log is unavailable. The
// packaged launcher sends process stdout/stderr to /dev/null, so invoke the
// macOS unified-log bridge explicitly as well as preserving direct-run stderr.
func reportFallback(message string) {
	if path, err := exec.LookPath("logger"); err == nil {
		_ = exec.Command(path, "-t", productName, message).Run()
	}
	fmt.Fprintf(os.Stderr, "%s: %s\n", productName, message)
}

// verifyCUPSBinaries fails fast when the CUPS client tools are missing. This is
// the one fatal startup condition: without lp/lpstat the agent could only ever
// report phantom success, which is the exact failure mode it exists to kill.
func verifyCUPSBinaries() error {
	for _, binary := range []string{"lp", "lpstat"} {
		if _, err := exec.LookPath(binary); err != nil {
			return fmt.Errorf(
				"%q not found on PATH; the CUPS client tools are required", binary)
		}
	}
	return nil
}

// fileExists reports whether path is a readable regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
