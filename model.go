/*
 * Copyright 2024 RapidLoop, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/jackc/pgx/v5"
	"github.com/robfig/cron/v3"
)

type Config struct {
	Listen     string           `hcl:"listen"`
	Connection ConnectionConfig `hcl:"connection,block"`
	Endpoints  []EndpointConfig `hcl:"endpoint,block"`
}

func (c *Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen: %v", err)
	}
	if err := c.Connection.Validate(); err != nil {
		return err
	}
	if len(c.Endpoints) == 0 {
		return errors.New("no endpoints defined")
	}
	for _, e := range c.Endpoints {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type ConnectionConfig struct {
	DSN         string `hcl:"dsn"`
	MaxConns    int    `hcl:"maxconns"`
	IdleTimeout string `hcl:"idletimeout"`
}

func (c *ConnectionConfig) Validate() error {
	if _, err := pgx.ParseConfig(c.DSN); err != nil {
		return fmt.Errorf("connection.dsn: %v", err)
	}
	if c.MaxConns < 0 {
		return errors.New("connection.maxconns: cannot be negative")
	}
	if c.IdleTimeout != "" {
		if _, err := time.ParseDuration(c.IdleTimeout); err != nil {
			return fmt.Errorf("connection.idletimeout: %v", err)
		}
	}
	return nil
}

type EndpointConfig struct {
	Path       string `hcl:"path,label"`
	SQL        string `hcl:"sql"`
	SQLTimeout string `hcl:"sqltimeout"`
	Schedule   string `hcl:"schedule"`
	RowFormat  string `hcl:"rowformat"`
	FileBacked bool   `hcl:"filebacked"`
}

var rxURI = regexp.MustCompile(`^(/(({[A-Za-z0-9_.-]+})|([A-Za-z0-9_.-]+)))+$`)

func (e *EndpointConfig) Validate() error {
	if !rxURI.MatchString(e.Path) && e.Path != "/" {
		return fmt.Errorf("endpoint \"%s\": invalid path: must be set to a valid URI", e.Path)
	}
	if strings.TrimSpace(e.SQL) == "" {
		return fmt.Errorf("endpoint \"%s\": invalid sql: must be set", e.Path)
	}
	if e.SQLTimeout != "" {
		if _, err := time.ParseDuration(e.SQLTimeout); err != nil {
			return fmt.Errorf("endpoint \"%s\": invalid sqltimeout: %v", e.Path, err)
		}
	}
	if _, err := cron.ParseStandard(e.Schedule); err != nil {
		return fmt.Errorf("endpoint \"%s\": invalid schedule: %v", e.Path, err)
	}
	if e.RowFormat != "" && e.RowFormat != "array" && e.RowFormat != "object" {
		return fmt.Errorf("endpoint \"%s\": invalid rowformat: must be 'array' or 'object'", e.Path)
	}
	return nil
}

func ConfigFromFile(filename string) (*Config, error) {
	var cfg Config
	src, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	file, diags := hclsyntax.ParseConfig(src, filename, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, diags
	}

	diags = gohcl.DecodeBody(file.Body, nil, &cfg)
	if diags.HasErrors() {
		return nil, diags
	}
	return &cfg, nil
}

//------------------------------------------------------------------------------

type EndpointResult struct {
	QueriedAt    string // as a string to avoid formatting each time
	CacheControl string
	ValidUntil   time.Time
	ETag         string
	Result       []byte
	File         string // if not empty, result is in this file
}
