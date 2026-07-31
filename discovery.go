package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	// connectionUSB / connectionNetwork are the two transports the wire Device
	// reports. They are classified from the CUPS device URI scheme, which is the
	// only place the transport appears (queue status text is byte-identical for
	// USB and network queues).
	connectionUSB     = "usb"
	connectionNetwork = "network"

	// deviceTypePrinter / deviceProvider / deviceManufacturer / deviceVersion are
	// the diagnostic Device fields. Only name+uid are load-bearing for the SPA;
	// provider identifies this agent rather than the dev shim.
	deviceTypePrinter  = "printer"
	deviceProvider     = productName
	deviceManufacturer = "Zebra Technologies"
	deviceVersion      = 2

	// usbScheme is the one URI scheme that means "directly attached".
	usbScheme = "usb"

	// serialMarker marks a device URI that carries a hardware serial. Such a URI
	// identifies the physical unit across a queue rename or a second identical
	// printer, so it — and only it — is hashed into the uid.
	serialMarker = "serial="

	// uidHashBytes is how much of the SHA-256 digest becomes the uid. 16 bytes
	// (32 hex chars) is far past any practical collision risk for the handful of
	// printers on one station.
	uidHashBytes = 16
)

// printer is one discovered CUPS queue plus the classification the wire Device
// is built from.
type printer struct {
	Name       string
	UID        string
	Queue      string
	DeviceURI  string
	Connection string
}

// wireDevice is the frozen Browser Print Device shape the SPA parses. The field
// set and JSON names must not drift: browserPrint.ts pins uid in localStorage
// and echoes the whole object back on a write.
type wireDevice struct {
	Name         string `json:"name"`
	UID          string `json:"uid"`
	Connection   string `json:"connection"`
	DeviceType   string `json:"deviceType"`
	Version      int    `json:"version"`
	Provider     string `json:"provider"`
	Manufacturer string `json:"manufacturer"`
}

// device renders the printer as the wire Device the SPA consumes.
func (p printer) device() wireDevice {
	return wireDevice{
		Name:         p.Name,
		UID:          p.UID,
		Connection:   p.Connection,
		DeviceType:   deviceTypePrinter,
		Version:      deviceVersion,
		Provider:     deviceProvider,
		Manufacturer: deviceManufacturer,
	}
}

// classifyConnection reports the transport a CUPS device URI describes.
//
// It keys on the URI SCHEME and nothing else. `lpinfo -v` prefixes its lines
// with a class word ("direct usb://…", "network socket://…"), but `lpstat -v` —
// which is what enumerates existing QUEUES, and therefore what the agent calls
// — emits no such prefix, so the classifier must never depend on it. usb:// is
// USB; every other scheme (socket, ipp, ipps, dnssd, lpd, smb, http, https) is
// network.
func classifyConnection(deviceURI string) string {
	scheme, _, found := strings.Cut(deviceURI, "://")
	if !found {
		return connectionNetwork
	}
	if strings.EqualFold(strings.TrimSpace(scheme), usbScheme) {
		return connectionUSB
	}
	return connectionNetwork
}

// stableUID derives the identity the SPA pins in localStorage.
//
// The invariant is stability across restart, reprobe, health flap, discovery
// order, and hot-plug. A URI carrying serial= identifies the physical unit
// (surviving a queue rename and distinguishing two identical ZD621s on one
// station), so it is hashed RAW — byte for byte as `lpstat -v` reported it,
// with no percent-decoding, no case folding, and no query reordering, because
// a decode step whose behavior varies across CUPS/Go versions would silently
// shift a station's uids and orphan the pin. A queue whose URI carries no
// serial falls back to the queue name.
func stableUID(queue string, deviceURI string) string {
	if deviceURI == "" || !strings.Contains(deviceURI, serialMarker) {
		return queue
	}
	sum := sha256.Sum256([]byte(deviceURI))
	return hex.EncodeToString(sum[:uidHashBytes])
}

// discoverPrinters enumerates the station's CUPS queues in the order the agent
// prefers them: USB first, then network, each tier keeping CUPS's own order.
// Health is NOT consulted here — discovery answers "what exists", and the
// health pass filters it down to "what can print".
func discoverPrinters(ctx context.Context, cups *cupsClient) ([]printer, error) {
	devices, err := cups.deviceURIs(ctx)
	if err != nil {
		return nil, err
	}
	usb := make([]printer, 0, len(devices))
	network := make([]printer, 0, len(devices))
	for _, device := range devices {
		found := printer{
			Name:       device.Queue,
			UID:        stableUID(device.Queue, device.URI),
			Queue:      device.Queue,
			DeviceURI:  device.URI,
			Connection: classifyConnection(device.URI),
		}
		if found.Connection == connectionUSB {
			usb = append(usb, found)
		} else {
			network = append(network, found)
		}
	}
	return append(usb, network...), nil
}

// matchPrinter finds the printer a /write body names, by uid first and display
// name second (the SPA echoes a whole Device; a hand-rolled request may carry
// only a name).
func matchPrinter(printers []printer, requested string) (printer, bool) {
	if requested == "" {
		return printer{}, false
	}
	for _, candidate := range printers {
		if candidate.UID == requested {
			return candidate, true
		}
	}
	for _, candidate := range printers {
		if candidate.Name == requested {
			return candidate, true
		}
	}
	return printer{}, false
}
