package appservice

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/thiagoribeiro/nimlet/internal/domain"
)

// Registry holds all registered AppServices, indexed for fast lookup.
type Registry struct {
	all     []*domain.AppService
	byID    map[string]*domain.AppService
	byToken map[string]*domain.AppService // as_token → AS
}

// configFile mirrors the YAML structure of the AppServices config file.
type configFile struct {
	AppServices []struct {
		ID               string   `yaml:"id"`
		Namespace        string   `yaml:"namespace"` // namespace slug; "default" when omitted
		Endpoint         string   `yaml:"endpoint"`
		HSToken          string   `yaml:"hs_token"`
		ASToken          string   `yaml:"as_token"`
		SenderUsername   string   `yaml:"sender_username"`
		InterestedEvents []string `yaml:"interested_events"`
	} `yaml:"app_services"`
}

// LoadFromFile parses a YAML AppServices config file and returns a Registry.
func LoadFromFile(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("appservice: read config: %w", err)
	}

	var cf configFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("appservice: parse config: %w", err)
	}

	services := make([]*domain.AppService, 0, len(cf.AppServices))
	for _, raw := range cf.AppServices {
		if raw.ID == "" {
			return nil, fmt.Errorf("appservice: entry missing id")
		}
		if raw.Endpoint == "" {
			return nil, fmt.Errorf("appservice: %q missing endpoint", raw.ID)
		}
		if raw.HSToken == "" || raw.ASToken == "" {
			return nil, fmt.Errorf("appservice: %q missing hs_token or as_token", raw.ID)
		}
		if raw.SenderUsername == "" {
			return nil, fmt.Errorf("appservice: %q missing sender_username", raw.ID)
		}
		ns := raw.Namespace
		if ns == "" {
			ns = domain.DefaultNamespaceSlug
		}
		services = append(services, &domain.AppService{
			ID:               raw.ID,
			NamespaceSlug:    ns,
			Endpoint:         raw.Endpoint,
			HSToken:          raw.HSToken,
			ASToken:          raw.ASToken,
			SenderUsername:   raw.SenderUsername,
			InterestedEvents: raw.InterestedEvents,
		})
	}
	return New(services...)
}

// New builds a Registry, rejecting ambiguous configurations: a duplicate id or a shared
// as_token would let one AppService's credentials resolve to another.
func New(services ...*domain.AppService) (*Registry, error) {
	reg := &Registry{
		all:     make([]*domain.AppService, 0, len(services)),
		byID:    make(map[string]*domain.AppService, len(services)),
		byToken: make(map[string]*domain.AppService, len(services)),
	}
	for _, as := range services {
		if _, dup := reg.byID[as.ID]; dup {
			return nil, fmt.Errorf("appservice: duplicate id %q", as.ID)
		}
		if _, dup := reg.byToken[as.ASToken]; dup {
			return nil, fmt.Errorf("appservice: %q reuses another entry's as_token", as.ID)
		}
		if as.NamespaceSlug != "" && !domain.ValidNamespaceSlug(as.NamespaceSlug) {
			return nil, fmt.Errorf("appservice: %q has invalid namespace %q", as.ID, as.NamespaceSlug)
		}
		reg.all = append(reg.all, as)
		reg.byID[as.ID] = as
		reg.byToken[as.ASToken] = as
	}
	return reg, nil
}

// Empty returns a Registry with no AppServices registered.
func Empty() *Registry {
	return &Registry{
		all:     nil,
		byID:    map[string]*domain.AppService{},
		byToken: map[string]*domain.AppService{},
	}
}

// All returns all registered AppServices.
func (r *Registry) All() []*domain.AppService {
	return r.all
}

// GetByID returns the AppService with the given ID, or false if not found.
func (r *Registry) GetByID(id string) (*domain.AppService, bool) {
	as, ok := r.byID[id]
	return as, ok
}

// GetByToken returns the AppService whose as_token matches, or false if not found.
func (r *Registry) GetByToken(token string) (*domain.AppService, bool) {
	as, ok := r.byToken[token]
	return as, ok
}
