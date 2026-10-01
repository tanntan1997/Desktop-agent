package device

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/ababank/mobile-device-agent/internal/events"
	"github.com/ababank/mobile-device-agent/internal/protocol"
)

// Event names published on the bus.
const (
	EventAdded   = "device.added"
	EventUpdated = "device.updated"
	EventRemoved = "device.removed"
	EventChanged = "devices.changed" // full list, after any change
)

type entry struct {
	info   Info
	driver Driver
}

// Manager polls every driver, tracks attached devices and publishes changes.
type Manager struct {
	drivers  []Driver
	bus      *events.Bus
	log      *slog.Logger
	interval time.Duration

	refreshMu sync.Mutex
	mu        sync.RWMutex
	devices   map[string]*entry
	listErr   map[Platform]string
}

func NewManager(drivers []Driver, bus *events.Bus, log *slog.Logger, interval time.Duration) *Manager {
	return &Manager{
		drivers:  drivers,
		bus:      bus,
		log:      log,
		interval: interval,
		devices:  map[string]*entry{},
		listErr:  map[Platform]string{},
	}
}

// Run polls until ctx is cancelled, then detaches every device.
func (m *Manager) Run(ctx context.Context) {
	m.Refresh(ctx)
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			for s, e := range m.devices {
				if a, ok := e.driver.(Attacher); ok {
					a.Detach(s)
				}
			}
			m.mu.Unlock()
			return
		case <-t.C:
			m.Refresh(ctx)
		}
	}
}

// Refresh re-scans all drivers once.
func (m *Manager) Refresh(ctx context.Context) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	seen := map[string]bool{}
	changed := false
	for _, d := range m.drivers {
		lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		infos, err := d.List(lctx)
		cancel()
		m.noteListErr(d.Platform(), err)
		if err != nil {
			// A transient tool failure must not look like every device unplugging.
			m.mu.RLock()
			for s, e := range m.devices {
				if e.driver == d {
					seen[s] = true
				}
			}
			m.mu.RUnlock()
			continue
		}
		for _, in := range infos {
			seen[in.Serial] = true
			if m.upsert(ctx, d, in) {
				changed = true
			}
		}
	}

	m.mu.Lock()
	var removed []*entry
	for s, e := range m.devices {
		if !seen[s] {
			removed = append(removed, e)
			delete(m.devices, s)
		}
	}
	m.mu.Unlock()
	for _, e := range removed {
		changed = true
		if a, ok := e.driver.(Attacher); ok {
			a.Detach(e.info.Serial)
		}
		m.log.Info("device detached", "serial", e.info.Serial, "platform", e.info.Platform)
		m.bus.Publish(EventRemoved, e.info)
	}
	if changed {
		m.bus.Publish(EventChanged, m.List())
	}
}

// upsert records a listed device, describing it when it first comes online
// (or when a previous describe failed). It reports whether anything changed.
func (m *Manager) upsert(ctx context.Context, d Driver, in Info) bool {
	m.mu.RLock()
	cur, existed := m.devices[in.Serial]
	m.mu.RUnlock()
	if existed && cur.info.State == in.State && cur.info.Error == "" {
		return false
	}

	info := in
	if in.State == StateOnline {
		dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		full, err := d.Describe(dctx, in.Serial)
		cancel()
		if err != nil {
			info.Error = err.Error()
			if existed && cur.info.Error == info.Error && cur.info.State == in.State {
				return false
			}
		} else {
			if full.Transport == "" {
				full.Transport = in.Transport
			}
			full.State = in.State
			info = full
		}
	}

	m.mu.Lock()
	m.devices[in.Serial] = &entry{info: info, driver: d}
	m.mu.Unlock()

	if a, ok := d.(Attacher); ok {
		wasOnline := existed && cur.info.State == StateOnline
		switch {
		case info.State == StateOnline && !wasOnline:
			a.Attach(info)
		case info.State != StateOnline && wasOnline:
			a.Detach(info.Serial)
		}
	}

	if existed {
		m.log.Info("device updated", "serial", info.Serial, "state", info.State, "error", info.Error)
		m.bus.Publish(EventUpdated, info)
	} else {
		m.log.Info("device attached", "serial", info.Serial, "platform", info.Platform,
			"model", info.Model, "os", info.OSVersion, "state", info.State, "error", info.Error)
		m.bus.Publish(EventAdded, info)
	}
	return true
}

func (m *Manager) noteListErr(p Platform, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	m.mu.Lock()
	prev := m.listErr[p]
	m.listErr[p] = msg
	m.mu.Unlock()
	if msg != prev {
		if msg != "" {
			m.log.Warn("device discovery failing", "platform", p, "err", msg)
		} else if prev != "" {
			m.log.Info("device discovery recovered", "platform", p)
		}
	}
}

// List returns a snapshot of all known devices, sorted by platform then serial.
func (m *Manager) List() []Info {
	m.mu.RLock()
	out := make([]Info, 0, len(m.devices))
	for _, e := range m.devices {
		out = append(out, e.info)
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Platform != out[j].Platform {
			return out[i].Platform < out[j].Platform
		}
		return out[i].Serial < out[j].Serial
	})
	return out
}

// DiscoveryErrors reports the latest per-platform discovery error, if any.
func (m *Manager) DiscoveryErrors() map[Platform]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[Platform]string{}
	for p, e := range m.listErr {
		if e != "" {
			out[p] = e
		}
	}
	return out
}

// Get returns a device that is online and ready for commands.
func (m *Manager) Get(serial string) (Info, Driver, error) {
	if serial == "" {
		return Info{}, nil, protocol.Errorf(protocol.CodeBadRequest, "serial is required")
	}
	m.mu.RLock()
	e, ok := m.devices[serial]
	m.mu.RUnlock()
	if !ok {
		return Info{}, nil, protocol.Errorf(protocol.CodeNotFound, "device %q is not connected", serial)
	}
	if e.info.State != StateOnline {
		return e.info, nil, protocol.Errorf(protocol.CodeUnavailable, "device %q is %s", serial, e.info.State)
	}
	return e.info, e.driver, nil
}
