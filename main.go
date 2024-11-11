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
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"
	"github.com/gorilla/handlers"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ovh/symmecrypt"
	_ "github.com/ovh/symmecrypt/ciphers/aesgcm"
	"github.com/robfig/cron/v3"
	"github.com/spf13/pflag"
)

// command-line flags
var (
	flagVersion bool
	flagExample bool
	flagHelp    bool
	flagDebug   bool
)

// filled in from command-line during build
var (
	version string // git tag --points-at HEAD
	githead string // git rev-parse --short HEAD
)

//go:embed example.cfg
var exampleCfg string

var (
	cryptKey symmecrypt.Key // key for encrypted files
	crond    *cron.Cron     // the cron "daemon"
	pool     *pgxpool.Pool  // the postgres connection pool
	cache    sync.Map       // the cache
)

const defaultMaxConns = 5 // default value for 'maxconns' in config

func printUsage(r io.Writer) {
	fmt.Fprintf(os.Stderr, `Usage: ellycache [-e] [-v] [-h] [config-file]

  ellycache -e > cache.cfg  # write an example config to cache.cfg
  ellycache cache.cfg       # start ellycache with cache.cfg config file

`)
	pflag.PrintDefaults()
	fmt.Fprintln(r)
	printVersion(r)
}

func printVersion(r io.Writer) {
	fmt.Fprintf(r, "ellycache %s %s\n(c) RapidLoop, Inc. 2024 * https://github.com/rapidloop/ellycache\n",
		version, githead)
}

func main() {
	os.Exit(realmain())
}

func realmain() int {
	// parse command line args
	pflag.BoolVarP(&flagVersion, "version", "v", false, "Print version and exit")
	pflag.BoolVarP(&flagExample, "example", "e", false, "Print example configuration to stdout and exit")
	pflag.BoolVarP(&flagHelp, "help", "h", false, "Print this help message")
	pflag.BoolVarP(&flagDebug, "debug", "d", false, "Enable debug logging")
	pflag.CommandLine.SortFlags = false
	pflag.Usage = func() { printUsage(os.Stderr) }
	pflag.Parse()
	if flagVersion {
		printVersion(os.Stdout)
		return 0
	}
	if flagExample {
		io.WriteString(os.Stdout, exampleCfg)
		return 0
	}
	if flagHelp {
		printUsage(os.Stdout)
		return 0
	}
	if pflag.NArg() != 1 {
		printUsage(os.Stderr)
		return 1
	}

	// read and validate config file
	cfgFile := pflag.Arg(0)
	cfg, err := ConfigFromFile(cfgFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ellycache: error reading config file %s: %v\n", cfgFile, err)
		return 1
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "ellycache: error in config file %s: %v\n", cfgFile, err)
		return 1
	}

	// create encryption key
	if cryptKey, err = symmecrypt.NewRandomKey("aes-gcm"); err != nil {
		fmt.Fprintf(os.Stderr, "ellycache: failed to create encryption key: %v\n", err)
		return 1
	}

	// connect to postgres
	if pgcfg, err := makePgxCfg(&cfg.Connection); err != nil {
		fmt.Fprintf(os.Stderr, "ellycache: error in connection config: %v\n", err)
		return 1
	} else if pool, err = pgxpool.NewWithConfig(context.Background(), pgcfg); err != nil {
		fmt.Fprintf(os.Stderr, "ellycache: failed to connect: %v\n", err)
		return 1
	}
	defer pool.Close()

	// start cron
	crond = cron.New()
	for _, e := range cfg.Endpoints {
		job := &Job{endpoint: &e}
		if entryID, err := crond.AddJob(e.Schedule, job); err != nil {
			fmt.Fprintf(os.Stderr, "ellycache: bad schedule \"%s\" in endpoint \"%s\"\n", e.Schedule, e.Path)
			return 1
		} else {
			job.entryID = entryID
			if flagDebug {
				if s, err := cron.ParseStandard(e.Schedule); err == nil {
					log.Printf("debug: scheduled %s, next at %s", e.Path,
						s.Next(time.Now()).Format("2006-01-02 15:04:05"))
				}
			}
		}
	}
	crond.Start()
	defer func() {
		ctx := crond.Stop()
		<-ctx.Done()
	}()

	// configure an http mux with the endpoints, wrap in compress handler
	mux := http.NewServeMux()
	for _, e := range cfg.Endpoints {
		mux.HandleFunc("GET "+e.Path, httpHandler)
	}
	handler := handlers.CompressHandler(mux)

	// start an http server
	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
	}
	if err := srv.ListenAndServe(); err != nil {
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "ellycache: http server error: %v\n", err)
			return 1
		}
	}

	return 0
}

// makePgxCfg creates a pgxpool.Config from a ConnectionConfig.
func makePgxCfg(c *ConnectionConfig) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(c.DSN)
	if err != nil {
		return nil, err
	}
	if c.MaxConns > 0 {
		cfg.MaxConns = int32(c.MaxConns)
	} else {
		cfg.MaxConns = defaultMaxConns
	}
	if d, err := time.ParseDuration(c.IdleTimeout); err == nil && d > 0 {
		cfg.MaxConnIdleTime = d
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	return cfg, nil
}

// Job is the cron job handler for an endpoint.
type Job struct {
	entryID  cron.EntryID
	endpoint *EndpointConfig
}

// Run is the cron job handler for an endpoint. If the query is successful, it
// updates the cache.
func (j *Job) Run() {
	// helper to delete old result's file
	cleanOld := func(oldResult *EndpointResult) {
		if oldResult.File != "" {
			if err := os.Remove(oldResult.File); err != nil {
				log.Printf("warning: failed to remove file %s: %v", oldResult.File, err)
			}
		}
	}

	// make new result
	newResult, err := makeResult(j.endpoint)
	if err != nil {
		log.Printf("warning: failed to query for %s: %v", j.endpoint.Path, err)
		if oldResult, loaded := cache.LoadAndDelete(j.endpoint.Path); loaded {
			or, _ := oldResult.(*EndpointResult)
			cleanOld(or)
			log.Printf("warning: removed old result for %s", j.endpoint.Path)
		}
		return
	}
	newResult.QueriedAt = time.Now().Truncate(time.Second).In(time.UTC).Format(http.TimeFormat)
	newResult.CacheControl = fmt.Sprintf("max-age=%d, immutable",
		int(crond.Entry(j.entryID).Next.Sub(time.Now()).Seconds()))

	// update cache, cleanup old entry if present
	oldResult, loaded := cache.Swap(j.endpoint.Path, newResult)
	if loaded {
		or, _ := oldResult.(*EndpointResult)
		cleanOld(or)
		if flagDebug {
			if or.ETag == newResult.ETag {
				log.Printf("debug: replaced result for %s (no change in content)", j.endpoint.Path)
			} else {
				log.Printf("debug: replaced result for %s", j.endpoint.Path)
			}
		}
	} else if flagDebug {
		log.Printf("debug: populated result for %s", j.endpoint.Path)
	}
}

// makeResult performs the query for the endpoint and returns the result.
func makeResult(e *EndpointConfig) (*EndpointResult, error) {
	if e.FileBacked {
		if f, err := os.CreateTemp("", "ellycache"); err != nil {
			return nil, fmt.Errorf("failed to create temp file: %v", err)
		} else {
			defer f.Close()
			w := symmecrypt.NewWriter(f, cryptKey)
			defer w.Close()
			if h, err := query(e, w); err != nil {
				return nil, err
			} else {
				return &EndpointResult{ETag: fmt.Sprintf(`W/"%x"`, h), File: f.Name()}, nil
			}
		}
	}

	b := &bytes.Buffer{}
	if h, err := query(e, b); err != nil {
		return nil, err
	} else {
		return &EndpointResult{ETag: fmt.Sprintf(`W/"%x"`, h), Result: b.Bytes()}, nil
	}
}

// query runs the SQL query for and endpoint and writes the result to the given
// io.Writer.
func query(e *EndpointConfig, w io.Writer) (uint64, error) {
	t1 := time.Now()
	defer func() {
		if flagDebug {
			log.Printf("debug: database query for %s took %v", e.Path, time.Since(t1))
		}
	}()

	digest := xxhash.New()

	ctx := context.Background()
	if d, err := time.ParseDuration(e.SQLTimeout); err == nil && d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	rows, err := pool.Query(ctx, e.SQL)
	if err != nil {
		return 0, fmt.Errorf("query failed: %v", err)
	}
	defer rows.Close()

	var columns []string
	if e.RowFormat != "array" {
		for _, fd := range rows.FieldDescriptions() {
			columns = append(columns, fd.Name)
		}
	}

	first := true
	fmt.Fprintln(w, "[")
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return 0, fmt.Errorf("query read failed: %v", err)
		}
		var row any
		if e.RowFormat == "array" {
			row = values
		} else {
			m := make(map[string]any)
			for i, v := range values {
				m[columns[i]] = v
			}
			row = m
		}
		j, err := json.Marshal(row)
		if err != nil {
			return 0, fmt.Errorf("failed to marshal to json: %v", err)
		}
		digest.Write(j)
		if first {
			first = false
		} else {
			fmt.Fprintln(w, ",")
		}
		fmt.Fprint(w, "  ")
		fmt.Fprint(w, string(j))
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("query error: %v", err)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "]")
	return digest.Sum64(), nil
}

// httpHandler serves up content for an endpoint.
func httpHandler(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	code := 200
	t1 := time.Now()
	defer func() {
		if flagDebug {
			log.Printf("debug: %q %d %v", r.URL.Path, code, time.Since(t1))
		}
	}()

	// load result
	v, ok := cache.Load(r.URL.Path)
	if !ok || v == nil {
		w.Header().Set("Cache-Control", "no-cache, no-store")
		code = http.StatusNotFound
		http.NotFound(w, r)
		return
	}
	result, _ := v.(*EndpointResult)
	sethdr := func() {
		hdr.Set("ETag", result.ETag)
		hdr.Set("Last-Modified", result.QueriedAt)
		hdr.Set("Cache-Control", result.CacheControl)
	}

	// check etag
	if r.Header.Get("If-None-Match") == result.ETag {
		code = http.StatusNotModified
		sethdr()
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// serve from memory
	if result.File == "" {
		hdr.Set("Content-Type", "application/json")
		sethdr()
		w.Write(result.Result)
		return
	}

	// serve from file
	f, err := os.Open(result.File)
	if err != nil {
		hdr.Set("Cache-Control", "no-cache, no-store")
		code = http.StatusInternalServerError
		log.Printf("http: %s: failed to open file %s: %v", r.URL.Path, result.File, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fr, err := symmecrypt.NewReader(f, cryptKey)
	if err != nil {
		hdr.Set("Cache-Control", "no-cache, no-store")
		code = http.StatusInternalServerError
		log.Printf("http: %s: failed to decrypt file %s: %v", r.URL.Path, result.File, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	hdr.Set("Content-Type", "application/json")
	sethdr()
	if _, err := io.Copy(w, fr); err != nil {
		code = http.StatusInternalServerError
		log.Printf("http: %s: failed to write response: %v", r.URL.Path, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}
