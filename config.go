package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
)

const (
	// defaultBind is loopback and stays loopback on a station: the agent is a
	// bridge between the local browser and local CUPS, never a network service.
	defaultBind = "127.0.0.1"

	// defaultPort / defaultHTTPSPort are the ports the frozen transport probes.
	// Chromium-family browsers reach the plain port directly (loopback is exempt
	// from mixed-content blocking); Safari needs the TLS one.
	defaultPort      = 9100
	defaultHTTPSPort = 9101

	// certFileName / keyFileName are the per-station pair the installer
	// generates. Their presence is what gates the HTTPS listener.
	certFileName = "cert.pem"
	keyFileName  = "key.pem"
)

// Environment mirrors of the flags, so a LaunchAgent can configure the station
// either through ProgramArguments (the canonical surface) or an env file.
//
// The names are built from envPrefix, so they are `BROWSER_PRINT_AGENTD_BIND`
// and friends and follow the product name automatically. There is deliberately
// no reader for the pre-extraction variable namespace: the agent is configured
// by a LaunchAgent plist the installer writes, so a silent compatibility
// fallback would only keep a stale variable working long enough to be
// forgotten about.
var (
	bindEnvVar        = envPrefix + "_BIND"
	portEnvVar        = envPrefix + "_PORT"
	httpsPortEnvVar   = envPrefix + "_HTTPS_PORT"
	certDirEnvVar     = envPrefix + "_CERT_DIR"
	originAllowEnvVar = envPrefix + "_ORIGIN_ALLOW"
)

// Package wiring rendered into the root-owned LaunchAgent plist. These are not
// station configuration: they tell the binary where the updater publication
// lives and which system-domain label launchd is authoritative for.
var (
	updateStatusEnvVar = envPrefix + "_UPDATE_STATUS_PATH"
	updaterLabelEnvVar = envPrefix + "_UPDATER_LABEL"
)

// config is the resolved runtime configuration. Ports and bind address are
// overridable per station without editing code; printer selection is NOT
// configurable — the agent discovers queues from CUPS, unlike the dev shim's
// hand-listed queue names.
type config struct {
	Bind             string
	Port             int
	HTTPSPort        int
	CertDir          string
	OriginAllow      []string
	UpdateStatusPath string
	UpdaterLabel     string
}

// certPaths returns the cert/key pair the HTTPS listener needs.
func (c config) certPaths() (string, string) {
	return filepath.Join(c.CertDir, certFileName), filepath.Join(c.CertDir, keyFileName)
}

// parseConfig resolves flags over environment defaults. A flag always wins over
// its environment mirror, which always wins over the built-in default.
func parseConfig(args []string, env func(string) string, output io.Writer) (config, error) {
	defaults := config{
		Bind:             envString(env, bindEnvVar, defaultBind),
		Port:             envInt(env, portEnvVar, defaultPort),
		HTTPSPort:        envInt(env, httpsPortEnvVar, defaultHTTPSPort),
		CertDir:          envString(env, certDirEnvVar, defaultCertDir(env)),
		UpdateStatusPath: env(updateStatusEnvVar),
		UpdaterLabel:     env(updaterLabelEnvVar),
	}
	originAllow := env(originAllowEnvVar)

	flags := flag.NewFlagSet(productName, flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&defaults.Bind, "bind", defaults.Bind,
		"address to bind the listeners to; keep loopback on a station")
	flags.IntVar(&defaults.Port, "port", defaults.Port, "plain HTTP port")
	flags.IntVar(&defaults.HTTPSPort, "https-port", defaults.HTTPSPort,
		"TLS port, served only when the cert pair exists")
	flags.StringVar(&defaults.CertDir, "cert-dir", defaults.CertDir,
		"directory holding "+certFileName+" and "+keyFileName)
	flags.StringVar(&originAllow, "origin-allow", originAllow,
		"comma-separated Origin allowlist; when set, a /write from any other "+
			"origin is rejected before any lp call. Unset means every origin is "+
			"logged and allowed")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if defaults.Port < 1 || defaults.Port > 65535 {
		return config{}, fmt.Errorf("invalid --port %d", defaults.Port)
	}
	if defaults.HTTPSPort < 1 || defaults.HTTPSPort > 65535 {
		return config{}, fmt.Errorf("invalid --https-port %d", defaults.HTTPSPort)
	}
	defaults.OriginAllow = parseOriginAllow(originAllow)
	return defaults, nil
}

// defaultCertDir is the per-user Application Support path the installer writes
// the station cert into. The agent is a per-user LaunchAgent, so this path is
// per-account by design.
func defaultCertDir(env func(string) string) string {
	home := env("HOME")
	if home == "" {
		return filepath.Join(".", productName)
	}
	return filepath.Join(home, "Library", "Application Support", productName)
}

// envString reads an environment override, falling back to fallback.
func envString(env func(string) string, name string, fallback string) string {
	if value := env(name); value != "" {
		return value
	}
	return fallback
}

// envInt reads a numeric environment override, ignoring an unparseable value so
// a typo in a plist degrades to the default rather than refusing to start.
func envInt(env func(string) string, name string, fallback int) int {
	value := env(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
