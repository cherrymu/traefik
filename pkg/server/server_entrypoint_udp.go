package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/traefik/traefik/v3/pkg/config/static"
	"github.com/traefik/traefik/v3/pkg/logs"
	"github.com/traefik/traefik/v3/pkg/udp"
)

// UDPEntryPoints maps UDP entry points by their names.
type UDPEntryPoints map[string]*UDPEntryPoint

// NewUDPEntryPoints returns all the UDP entry points, keyed by name.
func NewUDPEntryPoints(config static.EntryPoints) (UDPEntryPoints, error) {
	entryPoints := make(UDPEntryPoints)
	for entryPointName, entryPoint := range config {
		protocol, err := entryPoint.GetProtocol()
		if err != nil {
			return nil, fmt.Errorf("error while building entryPoint %s: %w", entryPointName, err)
		}

		if protocol != "udp" {
			continue
		}

		ep, err := NewUDPEntryPoint(entryPoint, entryPointName)
		if err != nil {
			return nil, fmt.Errorf("error while building entryPoint %s: %w", entryPointName, err)
		}
		entryPoints[entryPointName] = ep
	}
	return entryPoints, nil
}

// Start commences the listening for all the entry points.
func (eps UDPEntryPoints) Start() {
	for entryPointName, ep := range eps {
		ctx := log.With().Str(logs.EntryPointName, entryPointName).Logger().WithContext(context.Background())
		go ep.Start(ctx)
	}
}

// Stop makes all the entry points stop listening, and release associated resources.
func (eps UDPEntryPoints) Stop() {
	var wg sync.WaitGroup

	for epn, ep := range eps {
		wg.Add(1)

		go func(entryPointName string, entryPoint *UDPEntryPoint) {
			defer wg.Done()

			logger := log.With().Str(logs.EntryPointName, entryPointName).Logger()
			entryPoint.Shutdown(logger.WithContext(context.Background()))

			logger.Debug().Msg("Entry point closed")
		}(epn, ep)
	}

	wg.Wait()
}

// Switch swaps out all the given handlers in their associated entrypoints.
func (eps UDPEntryPoints) Switch(handlers map[string]udp.Handler) {
	for epName, handler := range handlers {
		if ep, ok := eps[epName]; ok {
			ep.Switch(handler)
			continue
		}

		log.Error().Str(logs.EntryPointName, epName).Msg("EntryPoint does not exist")
	}
}

// UDPEntryPoint is an entry point where we listen for UDP packets.
type UDPEntryPoint struct {
	listener               *udp.Listener
	switcher               *udp.HandlerSwitcher
	transportConfiguration *static.EntryPointsTransport
}

// NewUDPEntryPoint returns a UDP entry point.
func NewUDPEntryPoint(config *static.EntryPoint, name string) (*UDPEntryPoint, error) {
	var listener *udp.Listener
	var err error

	timeout := time.Duration(config.UDP.Timeout)

	// if we have predefined connections from socket activation
	if socketActivation.isEnabled() {
		if conn, err := socketActivation.getConn(name); err == nil {
			listener, err = udp.ListenPacketConn(conn, timeout)
			if err != nil {
				log.Warn().Err(err).Str("name", name).Msg("Unable to create socket activation listener")
			}
		} else {
			log.Warn().Err(err).Str("name", name).Msg("Unable to use socket activation for entrypoint")
		}
	}

	if listener == nil {
		listenConfig := newListenConfig(config)
		listener, err = udp.Listen(listenConfig, "udp", config.GetAddress(), timeout)
		if err != nil {
			return nil, fmt.Errorf("error creating listener: %w", err)
		}
	}

	return &UDPEntryPoint{listener: listener, switcher: &udp.HandlerSwitcher{}, transportConfiguration: config.Transport}, nil
}

// Start commences the listening for ep.
func (ep *UDPEntryPoint) Start(ctx context.Context) {
	log.Ctx(ctx).Debug().Msg("Start UDP Server")
	for {
		conn, err := ep.listener.Accept()
		if err != nil {
			// Only errClosedListener can happen that's why we return
			return
		}

		go ep.switcher.ServeUDP(conn)
	}
}

// Shutdown closes ep's listener. It eventually closes all "sessions" and
// releases associated resources, but only after it has waited for a graceTimeout,
// if any was configured.
func (ep *UDPEntryPoint) Shutdown(ctx context.Context) {
	logger := log.Ctx(ctx)

	reqAcceptGraceTimeOut := time.Duration(ep.transportConfiguration.LifeCycle.RequestAcceptGraceTimeout)
	if reqAcceptGraceTimeOut > 0 {
		logger.Info().Msgf("Waiting %s for incoming requests to cease", reqAcceptGraceTimeOut)
		time.Sleep(reqAcceptGraceTimeOut)
	}

	graceTimeOut := time.Duration(ep.transportConfiguration.LifeCycle.GraceTimeOut)
	if err := ep.listener.Shutdown(graceTimeOut); err != nil {
		logger.Error().Err(err).Send()
	}
}

// Switch replaces ep's handler with the one given as argument.
func (ep *UDPEntryPoint) Switch(handler udp.Handler) {
	ep.switcher.Switch(handler)
}
