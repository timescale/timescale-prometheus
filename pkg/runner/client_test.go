// This file and its contents are licensed under the Apache License 2.0.
// Please see the included NOTICE for copyright information and
// LICENSE for a copy of the license.

package runner

import (
	"flag"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/timescale/promscale/pkg/api"
	"github.com/timescale/promscale/pkg/log"
)

func TestMain(m *testing.M) {
	flag.Parse()
	err := log.Init(log.Config{
		Level: "debug",
	})

	if err != nil {
		fmt.Println("Error initializing logger", err)
		os.Exit(1)
	}
	code := m.Run()
	os.Exit(code)
}

func TestInitElector(t *testing.T) {
	// TODO: refactor the function to be fully testable without using a DB.
	testCases := []struct {
		name         string
		cfg          *Config
		shouldError  bool
		electionType reflect.Type
	}{
		{
			name: "Cannot create scheduled elector, no group lock ID and not rest election",
			cfg: &Config{
				HaGroupLockID: 0,
			},
		},
		{
			name: "Prometheus timeout not set for PG advisory lock",
			cfg: &Config{
				HaGroupLockID:     1,
				PrometheusTimeout: -1,
			},
			shouldError: true,
		},
		{
			name: "Can't get advisory lock, couldn't connect to DB",
			cfg: &Config{
				HaGroupLockID:     1,
				PrometheusTimeout: 0,
			},
			shouldError: true,
		},
	}
	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			metrics := api.InitMetrics(0)
			elector, err := initElector(c.cfg, metrics)

			switch {
			case err != nil && !c.shouldError:
				t.Errorf("Unexpected error, got %s", err.Error())
			case err == nil && c.shouldError:
				t.Errorf("Expected error, got nil")
			}

			if c.electionType != nil {
				if elector == nil {
					t.Fatalf("Expected to create elector, got nil")
				}

				v := reflect.ValueOf(elector).Elem().Field(0).Elem()

				if v.Type() != c.electionType {
					t.Errorf("Wrong type of elector created: got %v wanted %v", v.Type(), c.electionType)
				}
			}
		})
	}
}
