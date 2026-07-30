//go:build cgo

// This file holds the Wails-bound object: the frontend's entire view of Go.
//
// Owner: WP-8. WP-0 left this as compiling stubs.
//
// It is behind //go:build cgo with main.go because WP-8 will emit events through
// the Wails runtime, which is cgo-linked on Windows. Nothing in the internal
// packages needs cgo, so everything below this file is testable at Gate A.
//
// # The bound surface, which is frozen with the interfaces
//
//	ListInputDevices()        []gst.Device      caller: WP-5b
//	GetConfig()               *config.Config    caller: WP-5b
//	SaveConfig(c)             error             caller: WP-5b
//	Start() / Stop()          error             caller: WP-5b
//	GetKVSCredentials()       kvs.Credentials   caller: WP-5a
//
// # Events emitted Go to JS
//
//	"status"  an m2lx.Status
//	"sender"  a sender.State
//	"error"   a string
//
// Headphone enumeration and selection are JavaScript-side only, through
// enumerateDevices and setSinkId. No Go package owns output devices.
package main

import (
	"context"
	"errors"

	"wslcomms/internal/config"
	"wslcomms/internal/gst"
	"wslcomms/internal/kvs"
)

// Wails event names emitted from Go to the frontend. WP-5a and WP-5b subscribe
// to these, so the strings are part of the contract.
const (
	// EventStatus carries an m2lx.Status: the debounced, staleness-checked
	// switcher_status snapshot behind the SWITCHER SEES FEED, VIDEO and AUDIO
	// lamps.
	EventStatus = "status"

	// EventSender carries a sender.State behind the SENDING lamp.
	EventSender = "sender"

	// EventError carries a human-readable error string for display.
	EventError = "error"
)

// App is the object bound to the frontend. One instance exists for the life of
// the process.
type App struct {
	ctx context.Context
}

// NewApp creates the bound application object.
func NewApp() *App {
	return &App{}
}

// startup is the Wails OnStartup callback. It captures the context that the
// runtime's event functions require.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// ListInputDevices returns the audio capture endpoints for the commentary input
// dropdown. Device.ID is the IMMDevice endpoint GUID and is what SaveConfig must
// be given; Device.Name is for display only.
func (a *App) ListInputDevices() ([]gst.Device, error) {
	return nil, errors.New("not implemented: WP-8")
}

// GetConfig returns the current configuration for the Settings screen.
func (a *App) GetConfig() (*config.Config, error) {
	return nil, errors.New("not implemented: WP-8")
}

// SaveConfig persists the configuration. It does not restart a running session:
// changing the input device mid-match requires Stop then Start.
func (a *App) SaveConfig(c *config.Config) error {
	return errors.New("not implemented: WP-8")
}

// SetSecret writes one of the two Credential Manager secrets from the Settings
// screen. key is secrets.KeyM2LX or secrets.KeySRT. There is deliberately no
// getter: a secret goes into Credential Manager and never comes back out across
// this boundary.
func (a *App) SetSecret(key, value string) error {
	return errors.New("not implemented: WP-8")
}

// Start begins the contribution session: it builds the pipeline and enters the
// reconnect state machine. Progress is reported on the "sender" event, not by
// this method, which returns as soon as the pipeline is playing.
func (a *App) Start() error {
	return errors.New("not implemented: WP-8")
}

// Stop ends the contribution session.
func (a *App) Stop() error {
	return errors.New("not implemented: WP-8")
}

// GetKVSCredentials runs the M2L-X to Cognito chain and returns credentials for
// the monitor page. The page calls it again from scratch whenever the peer
// connection or the signalling socket dies; there is no refresh scheduler.
func (a *App) GetKVSCredentials() (kvs.Credentials, error) {
	return kvs.Credentials{}, errors.New("not implemented: WP-8")
}
