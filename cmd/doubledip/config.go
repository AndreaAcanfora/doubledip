package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/AndreaAcanfora/doubledip/internal/hunt"
)

// fileConfig is the doubledip.yml format.
type fileConfig struct {
	DB             string         `yaml:"db"`
	App            string         `yaml:"app"`
	Ready          string         `yaml:"ready"`
	Listen         string         `yaml:"listen"`
	BarrierTimeout time.Duration  `yaml:"barrier_timeout"`
	Max            int            `yaml:"max"`
	FreeRuns       *int           `yaml:"free_runs"`
	RestartApp     bool           `yaml:"restart_app"`
	AllowRemote    bool           `yaml:"allow_remote"`
	Scenarios      []fileScenario `yaml:"scenarios"`
}

type fileScenario struct {
	Name     string   `yaml:"name"`
	Request  string   `yaml:"request"`
	Requests []string `yaml:"requests"`
	Ignore   []string `yaml:"ignore"`
}

// loadConfig reads a config file. ${VAR} references in db, app, ready and
// listen are expanded from the environment; request commands are left
// alone, since the shell expands them when they run.
func loadConfig(path string) (*fileConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c fileConfig
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.DB, c.App, c.Ready, c.Listen = os.ExpandEnv(c.DB), os.ExpandEnv(c.App), os.ExpandEnv(c.Ready), os.ExpandEnv(c.Listen)
	for i, s := range c.Scenarios {
		if s.Name == "" {
			c.Scenarios[i].Name = fmt.Sprintf("scenario %d", i+1)
		}
		switch {
		case s.Request != "" && len(s.Requests) > 0:
			return nil, fmt.Errorf("%s: scenario %q has both request and requests", path, c.Scenarios[i].Name)
		case s.Request != "":
			c.Scenarios[i].Requests = []string{s.Request}
		case len(s.Requests) == 0 || len(s.Requests) > 2:
			return nil, fmt.Errorf("%s: scenario %q needs a request, or two requests to race against each other", path, c.Scenarios[i].Name)
		}
	}
	if len(c.Scenarios) == 0 {
		return nil, errors.New(path + ": no scenarios")
	}
	return &c, nil
}

func (s fileScenario) scenario() hunt.Scenario {
	return hunt.Scenario{Name: s.Name, Requests: s.Requests, Ignore: s.Ignore}
}
