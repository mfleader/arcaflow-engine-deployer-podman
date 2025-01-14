package podman

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	log "go.arcalot.io/log/v2"
	"go.flow.arcalot.io/deployer"
	"go.flow.arcalot.io/pluginsdk/schema"
	"go.flow.arcalot.io/podmandeployer/internal/cliwrapper"
)

// NewFactory creates a new factory for the Docker deployer.
func NewFactory() deployer.ConnectorFactory[*Config] {
	return &factory{}
}

type factory struct {
}

func (f factory) ID() string {
	return "podman"
}

func (f factory) ConfigurationSchema() *schema.TypedScopeSchema[*Config] {
	return Schema
}

func (f factory) Create(config *Config, logger log.Logger) (deployer.Connector, error) {
	podmanPath, err := binaryCheck(config.Podman.Path)
	if err != nil {
		return &Connector{}, fmt.Errorf("podman binary check failed with error: %w", err)
	}
	podman := cliwrapper.NewCliWrapper(podmanPath, logger)

	var rngSeed int64
	if config.Podman.RngSeed == 0 {
		rngSeed = time.Now().UnixNano()
	} else {
		rngSeed = config.Podman.RngSeed
	}
	rng := rand.New(rand.NewSource(rngSeed))

	var containerNamePrefix string
	if config.Podman.ContainerNamePrefix == "" {
		containerNamePrefix = "arcaflow_podman"
	} else {
		containerNamePrefix = config.Podman.ContainerNamePrefix
	}

	return &Connector{
		config:              config,
		logger:              logger,
		podmanCliWrapper:    podman,
		containerNamePrefix: containerNamePrefix,
		rng:                 rng,
		rngSeed:             rngSeed,
		lock:                &sync.Mutex{},
	}, nil
}

func binaryCheck(podmanPath string) (string, error) {
	if podmanPath == "" {
		podmanPath = "podman"
	}
	if !filepath.IsAbs(podmanPath) {
		podmanPathAbs, err := exec.LookPath(podmanPath)
		if err != nil {
			return "", fmt.Errorf("podman executable not found in a valid path with error: %w", err)

		}
		podmanPath = podmanPathAbs
	}
	if _, err := os.Stat(podmanPath); err != nil {
		return "", fmt.Errorf("podman binary not found with error: %w", err)
	}
	return podmanPath, nil
}
